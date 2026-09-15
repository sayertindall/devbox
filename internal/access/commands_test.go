package access

import (
	"bytes"
	"context"
	"encoding/json"
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

// instanceJSON is what gcloud reports for a described instance, with an external
// address only when the box has one.
func instanceJSON(t *testing.T, name, externalIP string) string {
	t.Helper()
	var accessConfigs []map[string]string
	if externalIP != "" {
		accessConfigs = []map[string]string{{"natIP": externalIP}}
	}
	instance := map[string]any{
		"name":   name,
		"zone":   "us-central1-a",
		"status": "RUNNING",
		"networkInterfaces": []map[string]any{
			{"networkIP": "10.0.0.2", "accessConfigs": accessConfigs},
		},
	}
	// One object: a describe reports one instance, and a list reports an array of
	// them, so the fixture has to be the shape the verb actually gets.
	data, err := json.Marshal(instance)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// openOn hands every command the same session, so a command test observes what
// the box would be asked to do.
func openOn(session Session) func(context.Context, cli.Deps, box.Name) (Session, error) {
	return func(context.Context, cli.Deps, box.Name) (Session, error) { return session, nil }
}

const tunnelProxy = "  ProxyCommand gcloud compute start-iap-tunnel %h %p --listen-on-stdin " +
	"--project=example-project --zone=us-central1-a --verbosity=warning"

func TestSSHConfigWritesATunnelEntryForABoxWithoutAnAddress(t *testing.T) {
	path := homeWithSSH(t)
	cloud := &gcloud.Fake{Reply: func([]string) (string, error) { return instanceJSON(t, "alpha", ""), nil }}
	deps, out, _ := testDeps(testConfig(), cloud)
	if err := commandFor(t, ops{}, "ssh-config").Run(context.Background(), deps, []string{"alpha"}); err != nil {
		t.Fatalf("ssh-config: %v", err)
	}
	wantArgv := []string{
		"compute", "instances", "describe", "alpha",
		"--zone=us-central1-a", "--project=example-project", "--format=json",
	}
	if got := cloud.Last(); !slices.Equal(got, wantArgv) {
		t.Errorf("gcloud argv = %q, want %q", got, wantArgv)
	}
	raw := read(t, path)
	if !strings.Contains(raw, tunnelProxy+"\n") {
		t.Errorf("the tunnel line is missing:\n%s", raw)
	}
	if !strings.Contains(raw, "  HostName alpha\n") {
		t.Errorf("the entry does not resolve the instance name the tunnel needs:\n%s", raw)
	}
	if !strings.Contains(out.String(), "devbox-alpha") {
		t.Errorf("output does not name the entry it wrote:\n%s", out.String())
	}
}

func TestSSHConfigUsesTheExternalAddressWhenTheBoxHasOne(t *testing.T) {
	path := homeWithSSH(t)
	cloud := &gcloud.Fake{Reply: func([]string) (string, error) { return instanceJSON(t, "alpha", "34.5.6.7"), nil }}
	deps, _, _ := testDeps(testConfig(), cloud)
	if err := commandFor(t, ops{}, "ssh-config").Run(context.Background(), deps, []string{"alpha"}); err != nil {
		t.Fatalf("ssh-config: %v", err)
	}
	raw := read(t, path)
	if !strings.Contains(raw, "  HostName 34.5.6.7\n") {
		t.Errorf("the external address is missing:\n%s", raw)
	}
	if strings.Contains(raw, "ProxyCommand") {
		t.Errorf("a box with an address needs no tunnel:\n%s", raw)
	}
}

func TestSSHConfigWritesNothingInADryRun(t *testing.T) {
	path := homeWithSSH(t)
	deps, _, _ := testDeps(testConfig(), &gcloud.Fake{})
	deps.DryRun = true
	err := commandFor(t, ops{}, "ssh-config").Run(context.Background(), deps, []string{"alpha"})
	if !errors.Is(err, gcloud.ErrDryRun) {
		t.Fatalf("error = %v, want the dry run sentinel", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a dry run created %s", path)
	}
}

func TestForwardPrintsAndOpensTheConfiguredTunnel(t *testing.T) {
	cloud := &gcloud.Fake{}
	deps, out, _ := testDeps(testConfig(), cloud)
	if err := commandFor(t, ops{}, "forward").Run(context.Background(), deps, []string{"alpha"}); err != nil {
		t.Fatalf("forward: %v", err)
	}
	wantArgv := []string{
		"compute", "start-iap-tunnel", "alpha", "9224",
		"--local-host-port=localhost:9224",
		"--project=example-project", "--zone=us-central1-a",
	}
	if got := cloud.Last(); !slices.Equal(got, wantArgv) {
		t.Errorf("gcloud argv = %q, want %q", got, wantArgv)
	}
	wantLine := "gcloud " + strings.Join(wantArgv, " ")
	if got := strings.TrimSpace(out.String()); got != wantLine {
		t.Errorf("printed\n%s\nwant\n%s", got, wantLine)
	}
}

func TestForwardUsesThePortTheOperatorNamesLast(t *testing.T) {
	cloud := &gcloud.Fake{}
	deps, out, _ := testDeps(testConfig(), cloud)
	if err := commandFor(t, ops{}, "forward").Run(context.Background(), deps, []string{"alpha", "--port", "3000"}); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if got := cloud.Last(); !slices.Contains(got, "--local-host-port=localhost:3000") {
		t.Errorf("gcloud argv = %q, want the named port", got)
	}
	if !strings.Contains(out.String(), "3000") {
		t.Errorf("the printed command does not show the named port:\n%s", out.String())
	}
	if err := commandFor(t, ops{}, "forward").Run(context.Background(), deps, []string{"alpha", "--port", "70000"}); err == nil {
		t.Error("a port outside the valid range must be refused")
	}
}

func TestForwardDefaultsToTheStandardPortWhenTheConfigurationNamesNone(t *testing.T) {
	cfg := testConfig()
	cfg.PortForward = 0
	cloud := &gcloud.Fake{}
	deps, _, _ := testDeps(cfg, cloud)
	if err := commandFor(t, ops{}, "forward").Run(context.Background(), deps, []string{"alpha"}); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if got := cloud.Last(); !slices.Contains(got, "--local-host-port=localhost:9224") {
		t.Errorf("gcloud argv = %q, want the standard port", got)
	}
}

func TestSSHStreamsTheOperatorsTerminal(t *testing.T) {
	recorder := &procRecorder{reply: func(procCall) (string, error) { return "builder@alpha:~$ \n", nil }}
	deps, out, _ := testDeps(testConfig(), &gcloud.Fake{})
	if err := commandFor(t, ops{open: openOn(nil), proc: recorder.run}, "ssh").Run(context.Background(), deps, []string{"alpha"}); err != nil {
		t.Fatalf("ssh: %v", err)
	}
	want := []string{"ssh", "devbox-alpha"}
	if len(recorder.calls) != 1 || !slices.Equal(recorder.calls[0].argv, want) {
		t.Fatalf("argv = %q, want %q", recorder.calls, want)
	}
	if out.String() != "builder@alpha:~$ \n" {
		t.Errorf("output = %q, want the session's output", out.String())
	}
}

func TestSSHRunsOneCommandWhenTheOperatorNamesOne(t *testing.T) {
	recorder := &procRecorder{}
	deps, _, _ := testDeps(testConfig(), &gcloud.Fake{})
	args := []string{"alpha", "--", "journalctl -u docker --no-pager | tail -5"}
	if err := commandFor(t, ops{open: openOn(nil), proc: recorder.run}, "ssh").Run(context.Background(), deps, args); err != nil {
		t.Fatalf("ssh: %v", err)
	}
	want := []string{"ssh", "devbox-alpha", "--", "journalctl -u docker --no-pager | tail -5"}
	if len(recorder.calls) != 1 || !slices.Equal(recorder.calls[0].argv, want) {
		t.Fatalf("argv = %q, want %q", recorder.calls, want)
	}
}

func TestExecRunsTheCommandThroughTheSession(t *testing.T) {
	session := &Recording{Reply: func(string) (string, error) { return "hello from the box\n", nil }}
	deps, out, _ := testDeps(testConfig(), &gcloud.Fake{})
	if err := commandFor(t, ops{open: openOn(session)}, "exec").Run(context.Background(), deps, []string{"alpha", "--", "echo hello"}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !slices.Equal(session.Commands, []string{"echo hello"}) {
		t.Errorf("commands = %q, want the operator's command", session.Commands)
	}
	if out.String() != "hello from the box\n" {
		t.Errorf("output = %q, want the command's output", out.String())
	}
}

func TestExecFailsWhenTheCommandFails(t *testing.T) {
	session := &Recording{Fail: "false"}
	deps, _, _ := testDeps(testConfig(), &gcloud.Fake{})
	err := commandFor(t, ops{open: openOn(session)}, "exec").Run(context.Background(), deps, []string{"alpha", "--", "false"})
	if err == nil {
		t.Fatal("a command that fails on the box must fail here")
	}
	if len(session.Commands) != 1 {
		t.Errorf("commands = %q, want one attempt", session.Commands)
	}
}

func TestExecNeedsACommand(t *testing.T) {
	deps, _, _ := testDeps(testConfig(), &gcloud.Fake{})
	if err := commandFor(t, ops{open: openOn(&Recording{})}, "exec").Run(context.Background(), deps, []string{"alpha"}); err == nil {
		t.Fatal("exec without a command must be refused")
	}
}

func TestCopyMovesAPathInEitherDirection(t *testing.T) {
	session := &Recording{}
	deps, out, _ := testDeps(testConfig(), &gcloud.Fake{})
	command := commandFor(t, ops{open: openOn(session)}, "cp")
	if err := command.Run(context.Background(), deps, []string{"alpha", "internal/access", "devbox/trees/main"}); err != nil {
		t.Fatalf("cp: %v", err)
	}
	if !slices.Equal(session.Uploads, [][2]string{{"internal/access", "devbox/trees/main"}}) {
		t.Errorf("uploads = %q, want the operator's paths", session.Uploads)
	}
	if err := command.Run(context.Background(), deps, []string{"alpha", "--down", "devbox/trees/main", "internal/access"}); err != nil {
		t.Fatalf("cp --down: %v", err)
	}
	if !slices.Equal(session.Downloads, [][2]string{{"devbox/trees/main", "internal/access"}}) {
		t.Errorf("downloads = %q, want the operator's paths", session.Downloads)
	}
	if !strings.Contains(out.String(), "uploaded internal/access to devbox-alpha:devbox/trees/main") {
		t.Errorf("output = %q, want the direction and the alias", out.String())
	}
}

func TestCopyRefusesAPathCountItCannotAct(t *testing.T) {
	deps, _, _ := testDeps(testConfig(), &gcloud.Fake{})
	err := commandFor(t, ops{open: openOn(&Recording{})}, "cp").Run(context.Background(), deps, []string{"alpha", "one"})
	if err == nil {
		t.Fatal("cp with one path must be refused")
	}
	if !strings.Contains(err.Error(), "usage: devbox cp") {
		t.Errorf("error = %v, want the usage", err)
	}
}

func TestTerminfoInstallsTheLocalEntryOnTheBox(t *testing.T) {
	const entry = "xterm-ghostty|ghostty|Ghostty,\n\tam, bce, ccc,\n"
	recorder := &procRecorder{reply: func(call procCall) (string, error) {
		if call.argv[0] == "infocmp" {
			return entry, nil
		}
		return "", nil
	}}
	deps, out, _ := testDeps(testConfig(), &gcloud.Fake{})
	if err := commandFor(t, ops{open: openOn(nil), proc: recorder.run}, "terminfo").Run(context.Background(), deps, []string{"alpha"}); err != nil {
		t.Fatalf("terminfo: %v", err)
	}
	want := [][]string{
		{"infocmp", "-x", "xterm-ghostty"},
		{"ssh", "-o", "BatchMode=yes", "devbox-alpha", "--", "tic -x -"},
	}
	if len(recorder.calls) != len(want) {
		t.Fatalf("calls = %q, want the entry read here and fed to tic on the box", recorder.calls)
	}
	for index, expected := range want {
		if !slices.Equal(recorder.calls[index].argv, expected) {
			t.Errorf("argv %d = %q, want %q", index, recorder.calls[index].argv, expected)
		}
	}
	if recorder.calls[1].stdin != entry {
		t.Errorf("tic received %q, want the entry", recorder.calls[1].stdin)
	}
	if !strings.Contains(out.String(), "SetEnv TERM=xterm-256color") {
		t.Errorf("output = %q, want the fallback line", out.String())
	}
}

func TestTerminfoPrefersTheHomebrewInfocmpWhenTheSystemOneFails(t *testing.T) {
	const entry = "xterm-ghostty|Ghostty,\n"
	recorder := &procRecorder{reply: func(call procCall) (string, error) {
		switch call.argv[0] {
		case "/opt/homebrew/opt/ncurses/bin/infocmp":
			return entry, nil
		case "infocmp":
			return "", errors.New("exit status 1")
		default:
			return "", nil
		}
	}}
	deps, _, _ := testDeps(testConfig(), &gcloud.Fake{})
	if err := commandFor(t, ops{open: openOn(nil), proc: recorder.run}, "terminfo").Run(context.Background(), deps, []string{"alpha"}); err != nil {
		t.Fatalf("terminfo: %v", err)
	}
	want := [][]string{
		{"infocmp", "-x", "xterm-ghostty"},
		{"/opt/homebrew/opt/ncurses/bin/infocmp", "-x", "xterm-ghostty"},
		{"ssh", "-o", "BatchMode=yes", "devbox-alpha", "--", "tic -x -"},
	}
	if len(recorder.calls) != len(want) {
		t.Fatalf("calls = %q, want the fallback infocmp and the install", recorder.calls)
	}
	for index, expected := range want {
		if !slices.Equal(recorder.calls[index].argv, expected) {
			t.Errorf("argv %d = %q, want %q", index, recorder.calls[index].argv, expected)
		}
	}
}

func TestTerminfoReportsTheFallbackWhenNoInfocmpHasTheEntry(t *testing.T) {
	recorder := &procRecorder{reply: func(procCall) (string, error) {
		return "", errors.New("unknown terminal type")
	}}
	deps, out, _ := testDeps(testConfig(), &gcloud.Fake{})
	err := commandFor(t, ops{open: openOn(nil), proc: recorder.run}, "terminfo").Run(context.Background(), deps, []string{"alpha"})
	if err == nil {
		t.Fatal("an unknown terminal must be reported")
	}
	if !strings.Contains(err.Error(), "xterm-ghostty") {
		t.Errorf("error = %v, want the terminal that could not be read", err)
	}
	if !strings.Contains(out.String(), "SetEnv TERM=xterm-256color") {
		t.Errorf("output = %q, want the fallback line", out.String())
	}
	for _, call := range recorder.calls {
		if call.argv[0] == "ssh" {
			t.Errorf("nothing should reach the box without an entry: %q", call.argv)
		}
	}
}

func TestEditorsPrintsTheZedURLAndTheRemoteSSHHost(t *testing.T) {
	deps, out, _ := testDeps(testConfig(), &gcloud.Fake{})
	if err := commandFor(t, ops{}, "editors").Run(context.Background(), deps, []string{"alpha"}); err != nil {
		t.Fatalf("editors: %v", err)
	}
	want := "zed ssh://devbox-alpha/home/builder\nVS Code Remote-SSH host: devbox-alpha\n"
	if out.String() != want {
		t.Errorf("output =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestEditorsOpensThePathTheOperatorNames(t *testing.T) {
	deps, out, _ := testDeps(testConfig(), &gcloud.Fake{})
	command := commandFor(t, ops{}, "editors")
	if err := command.Run(context.Background(), deps, []string{"alpha", "devbox/trees/main"}); err != nil {
		t.Fatalf("editors: %v", err)
	}
	if !strings.Contains(out.String(), "zed ssh://devbox-alpha/home/builder/devbox/trees/main\n") {
		t.Errorf("a relative path must be read from the box's home:\n%s", out.String())
	}
	out.Reset()
	if err := command.Run(context.Background(), deps, []string{"alpha", "/srv/work"}); err != nil {
		t.Fatalf("editors: %v", err)
	}
	if !strings.Contains(out.String(), "zed ssh://devbox-alpha/srv/work\n") {
		t.Errorf("an absolute path must be used as it is:\n%s", out.String())
	}
}

func TestAccessVerbsRefuseAMissingBoxName(t *testing.T) {
	for _, name := range []string{"ssh", "exec", "cp", "forward", "terminfo", "editors", "doctor", "ssh-config"} {
		t.Run(name, func(t *testing.T) {
			deps, _, _ := testDeps(testConfig(), &gcloud.Fake{})
			err := commandFor(t, ops{proc: (&procRecorder{}).run, open: openOn(&Recording{})}, name).
				Run(context.Background(), deps, nil)
			if err == nil {
				t.Fatalf("%s without a box must be refused", name)
			}
			if !strings.Contains(err.Error(), "usage: devbox "+name) {
				t.Errorf("error = %v, want the usage line", err)
			}
		})
	}
}

func TestCopyRehearsesWithoutOpeningASession(t *testing.T) {
	// A rehearsal must not describe the box, so the opener is wired to fail: if cp
	// called it, the test would fail rather than print the command.
	ops := ops{}
	ops.open = func(context.Context, cli.Deps, box.Name) (Session, error) {
		return nil, errors.New("a rehearsal must not open a session")
	}
	ops.proc = func(context.Context, []string, io.Reader, io.Writer, io.Writer) error { return nil }
	registry := cli.NewRegistry()
	registry.Add(commandSet(ops)...)
	var out bytes.Buffer
	deps := cli.Deps{Config: config.Default(), Out: &out, Err: &out, DryRun: true}
	if err := registry.Run(context.Background(), deps, []string{"cp", "dev", "/tmp/a", "/tmp/b"}); err != nil {
		t.Fatalf("a rehearsal must print and stop: %v", err)
	}
	if !strings.Contains(out.String(), "would run: rsync -a --relative /tmp/a devbox-dev:/tmp/b/") {
		t.Fatalf("the rehearsal must print the copy it would make:\n%s", out.String())
	}
}

// TestSSHRefreshesTheHostEntryForTheBox covers the step every verb that runs ssh
// itself has to take: the entry is written from what the cloud says right now, so
// an alias that exists in the configuration always points at the live box.
func TestSSHRefreshesTheHostEntryForTheBox(t *testing.T) {
	recorder := &procRecorder{}
	var reached []box.Name
	o := ops{
		open: func(_ context.Context, _ cli.Deps, name box.Name) (Session, error) {
			reached = append(reached, name)
			return nil, nil
		},
		proc: recorder.run,
	}
	deps, _, _ := testDeps(testConfig(), &gcloud.Fake{})
	if err := commandFor(t, o, "ssh").Run(context.Background(), deps, []string{"alpha", "--", "hostname"}); err != nil {
		t.Fatalf("ssh: %v", err)
	}
	if len(reached) != 1 || reached[0].String() != "alpha" {
		t.Fatalf("ssh ran without refreshing the Host entry for alpha: %v", reached)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("ssh ran %d local processes, want one", len(recorder.calls))
	}
}

// TestSSHDoesNotRunWhenTheBoxCannotBeReached proves a missing box fails as a
// missing box: ssh never runs, so the operator never sees a DNS error about a
// name that only exists inside the configuration.
func TestSSHDoesNotRunWhenTheBoxCannotBeReached(t *testing.T) {
	recorder := &procRecorder{}
	o := ops{
		open: func(context.Context, cli.Deps, box.Name) (Session, error) {
			return nil, errors.New("box alpha was not found in project example-project zone us-central1-a")
		},
		proc: recorder.run,
	}
	deps, _, _ := testDeps(testConfig(), &gcloud.Fake{})
	err := commandFor(t, o, "ssh").Run(context.Background(), deps, []string{"alpha"})
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("ssh with an unreachable box = %v, want the box's own error", err)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("ssh ran %d local processes for a box it could not reach", len(recorder.calls))
	}
}
