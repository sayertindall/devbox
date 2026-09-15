package access

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
)

// procCall is one recorded local process: its argument vector and, when the
// caller piped something in, the bytes the process received on standard input.
type procCall struct {
	argv  []string
	stdin string
}

// procRecorder stands in for the real local process runner so a test can assert
// the exact vector ssh or rsync would receive without running either.
type procRecorder struct {
	calls []procCall
	// reply returns the output and error for one call.
	reply func(call procCall) (string, error)
}

// run records the call and answers it, writing any output where the caller asked
// for it, which is how a captured run is observed.
func (p *procRecorder) run(_ context.Context, argv []string, in io.Reader, out, errOut io.Writer) error {
	call := procCall{argv: append([]string{}, argv...)}
	if in != nil {
		data, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		call.stdin = string(data)
	}
	p.calls = append(p.calls, call)
	if p.reply == nil {
		return nil
	}
	text, err := p.reply(call)
	if text != "" {
		if _, writeErr := io.WriteString(out, text); writeErr != nil {
			return writeErr
		}
	}
	return err
}

// testConfig is a complete configuration, so no test reads the operator's file
// or the environment.
func testConfig() config.Config {
	return config.Config{
		Project:     "example-project",
		Zone:        "us-central1-a",
		Region:      "us-central1",
		MachineType: "n2-standard-16",
		DataMount:   "/mnt/data",
		SSHPrefix:   "devbox-",
		RemoteUser:  "builder",
		PortForward: 9224,
	}
}

// testDeps returns command dependencies whose cloud is recorded and whose streams
// a test can read.
func testDeps(cfg config.Config, cloud gcloud.Executor) (cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	deps := cli.Deps{
		Config: cfg,
		Cloud:  cloud,
		Out:    out,
		Err:    errOut,
		Stdin:  strings.NewReader(""),
	}
	return deps, out, errOut
}

// commandFor returns one verb from the set built against the given operations.
func commandFor(t *testing.T, o ops, name string) cli.Command {
	t.Helper()
	for _, candidate := range commandSet(o) {
		if candidate.Name == name {
			return candidate
		}
	}
	t.Fatalf("no %s command", name)
	return cli.Command{}
}

// sessionOn returns a session whose transport is recorded, keeping everything the
// Dialer decided about the alias.
func sessionOn(t *testing.T, name box.Name, recorder *procRecorder) sshSession {
	t.Helper()
	opened, err := Dialer{Config: testConfig()}.Open(context.Background(), name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	session, ok := opened.(sshSession)
	if !ok {
		t.Fatalf("Open returned %T, want the ssh session", opened)
	}
	session.proc = recorder.run
	return session
}

func TestRunSendsTheCommandThroughTheAlias(t *testing.T) {
	recorder := &procRecorder{reply: func(procCall) (string, error) { return " 12:03  up 4 days\n", nil }}
	session := sessionOn(t, "alpha", recorder)
	out, err := session.Run(context.Background(), "uptime")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != " 12:03  up 4 days\n" {
		t.Errorf("output = %q, want the command's output", out)
	}
	want := []string{"ssh", "-o", "BatchMode=yes", "devbox-alpha", "--", "uptime"}
	if len(recorder.calls) != 1 || !slices.Equal(recorder.calls[0].argv, want) {
		t.Fatalf("argv = %q, want %q", recorder.calls, want)
	}
}

func TestRunReturnsOutputAndTheExitStatusOfAFailedCommand(t *testing.T) {
	recorder := &procRecorder{reply: func(procCall) (string, error) {
		return "systemctl: unit docker.service not found\n", errors.New("exit status 4")
	}}
	session := sessionOn(t, "alpha", recorder)
	out, err := session.Run(context.Background(), "systemctl stop docker")
	if err == nil {
		t.Fatal("a command that exits nonzero must fail")
	}
	if out != "systemctl: unit docker.service not found\n" {
		t.Errorf("output = %q, want what the command printed", out)
	}
	if !strings.Contains(err.Error(), "exit status 4") || !strings.Contains(err.Error(), "devbox-alpha") {
		t.Errorf("error = %v, want the alias and the exit status", err)
	}
}

func TestRunRefusesAnEmptyCommand(t *testing.T) {
	recorder := &procRecorder{}
	session := sessionOn(t, "alpha", recorder)
	if _, err := session.Run(context.Background(), "  "); err == nil {
		t.Fatal("an empty command must be refused")
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("nothing should reach the box, got %q", recorder.calls)
	}
}

func TestUploadAndDownloadArgv(t *testing.T) {
	recorder := &procRecorder{}
	session := sessionOn(t, "alpha", recorder)
	ctx := context.Background()
	if err := session.Upload(ctx, "internal/access/session.go", "devbox/trees/main"); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := session.Download(ctx, "devbox/agents/one/log", "/tmp/logs"); err != nil {
		t.Fatalf("download: %v", err)
	}
	want := [][]string{
		{"rsync", "-a", "--relative", "internal/access/session.go", "devbox-alpha:devbox/trees/main/"},
		{"rsync", "-a", "devbox-alpha:devbox/agents/one/log", "/tmp/logs/"},
	}
	if len(recorder.calls) != len(want) {
		t.Fatalf("calls = %q, want %d", recorder.calls, len(want))
	}
	for index, expected := range want {
		if !slices.Equal(recorder.calls[index].argv, expected) {
			t.Errorf("argv %d = %q, want %q", index, recorder.calls[index].argv, expected)
		}
		if slices.Contains(recorder.calls[index].argv, "--delete") {
			t.Errorf("argv %d uses --delete, which may not remove anything on the box", index)
		}
	}
}

func TestOpenRefusesANameThatIsNotABoxName(t *testing.T) {
	if _, err := (Dialer{Config: testConfig()}).Open(context.Background(), "Not A Box"); err == nil {
		t.Fatal("an invalid box name must be refused before anything is executed")
	}
}

func TestOpenWritesTheEntryFromTheBoxesAddressInTheCloud(t *testing.T) {
	path := homeWithSSH(t)
	cloud := &gcloud.Fake{Reply: func([]string) (string, error) { return instanceJSON(t, "alpha", "34.5.6.7"), nil }}
	dialer := Dialer{Config: testConfig(), Cloud: cloud}
	if _, err := dialer.Open(context.Background(), "alpha"); err != nil {
		t.Fatalf("open: %v", err)
	}
	raw := read(t, path)
	if !strings.Contains(raw, "  HostName 34.5.6.7\n") {
		t.Errorf("a session must use the address the box answers on:\n%s", raw)
	}
	if strings.Contains(raw, "ProxyCommand") {
		t.Errorf("a box with an address needs no tunnel:\n%s", raw)
	}
	want := []string{
		"compute", "instances", "describe", "alpha",
		"--zone=us-central1-a", "--project=example-project", "--format=json",
	}
	if got := cloud.Last(); !slices.Equal(got, want) {
		t.Errorf("gcloud argv = %q, want %q", got, want)
	}
}

func TestOpenWritesTheTunnelEntryForABoxWithoutAnAddress(t *testing.T) {
	path := homeWithSSH(t)
	cloud := &gcloud.Fake{Reply: func([]string) (string, error) { return instanceJSON(t, "alpha", ""), nil }}
	dialer := Dialer{Config: testConfig(), Cloud: cloud}
	ctx := context.Background()
	if _, err := dialer.Open(ctx, "alpha"); err != nil {
		t.Fatalf("open: %v", err)
	}
	first := read(t, path)
	if !strings.Contains(first, "  HostName alpha\n") || !strings.Contains(first, "  ProxyCommand gcloud compute start-iap-tunnel %h %p") {
		t.Errorf("a box with no address must be reached through the tunnel:\n%s", first)
	}
	if _, err := dialer.Open(ctx, "alpha"); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if second := read(t, path); second != first {
		t.Errorf("opening the same box twice rewrote the file:\n%s\n---\n%s", first, second)
	}
}

func TestOpenReplacesAnAddressTheBoxNoLongerHas(t *testing.T) {
	path := homeWithSSH(t)
	address := "10.0.0.9"
	cloud := &gcloud.Fake{Reply: func([]string) (string, error) { return instanceJSON(t, "alpha", address), nil }}
	dialer := Dialer{Config: testConfig(), Cloud: cloud}
	ctx := context.Background()
	if _, err := dialer.Open(ctx, "alpha"); err != nil {
		t.Fatalf("open: %v", err)
	}
	if raw := read(t, path); !strings.Contains(raw, "  HostName 10.0.0.9\n") {
		t.Fatalf("the first address is missing:\n%s", raw)
	}
	// The box was stopped and started while it kept its address, then it moved.
	address = "34.5.6.7"
	if _, err := dialer.Open(ctx, "alpha"); err != nil {
		t.Fatalf("second open: %v", err)
	}
	raw := read(t, path)
	if !strings.Contains(raw, "  HostName 34.5.6.7\n") {
		t.Errorf("the session would use a stale address:\n%s", raw)
	}
	if strings.Contains(raw, "10.0.0.9") {
		t.Errorf("the old address survived:\n%s", raw)
	}
}

// TestDialerDryRunLeavesTheSSHConfigurationAlone proves a rehearsal writes nothing
// on the operator's machine, which is the same promise the cloud mutations make.
func TestDialerDryRunLeavesTheSSHConfigurationAlone(t *testing.T) {
	path := homeWithSSH(t)
	var out bytes.Buffer
	cloud := &gcloud.Fake{Reply: func([]string) (string, error) { return instanceJSON(t, "alpha", ""), nil }}
	dialer := Dialer{Config: testConfig(), Cloud: cloud, DryRun: true, Out: &out}
	if _, err := dialer.Open(context.Background(), "alpha"); err != nil {
		t.Fatalf("open in a dry run: %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a dry run created %s", path)
	}
	if !strings.Contains(out.String(), "would refresh the Host entry") {
		t.Fatalf("the dry run did not say what it would write: %q", out.String())
	}
}
