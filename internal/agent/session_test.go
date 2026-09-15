package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"devbox/internal/box"
)

// TestLaunchCommandPerProvider pins the exact bytes devbox sends for one task
// holding a single quote, a double quote, and a dollar sign. Each harness is
// started the way its own help says it starts non-interactively, and the command
// crosses the login shell ssh starts and the shell tmux runs the session with, so
// the string is the contract. The round trip below proves the bytes mean what
// they say.
func TestLaunchCommandPerProvider(t *testing.T) {
	const ref = "0123456789abcdef"
	task := `fix it's "$HOME" now`
	const head = `tmux new-session -d -s devbox-0123456789abcdef ` +
		`'cd "$HOME/devbox/trees/work" && `
	const tail = `; echo done > "$HOME/devbox/agents/0123456789abcdef/status"'`

	want := []struct {
		provider Provider
		command  string
	}{
		{
			provider: ProviderOmp,
			command:  head + `omp -p '\''Read the handoff packet at '\''"$HOME/devbox/agents/0123456789abcdef/handoff.json"'\'' and work the task it describes: '\'''\''fix it'\''\'\'''\''s "$HOME" now'\''` + tail,
		},
		{
			provider: ProviderClaude,
			command:  head + `claude -p '\''Read the handoff packet at '\''"$HOME/devbox/agents/0123456789abcdef/handoff.json"'\'' and work the task it describes: '\'''\''fix it'\''\'\'''\''s "$HOME" now'\''` + tail,
		},
		{
			provider: ProviderCodex,
			command:  head + `codex exec '\''Read the handoff packet at '\''"$HOME/devbox/agents/0123456789abcdef/handoff.json"'\'' and work the task it describes: '\'''\''fix it'\''\'\'''\''s "$HOME" now'\''` + tail,
		},
	}
	if len(want) != len(harnesses) {
		t.Fatalf("the test covers %d providers and devbox starts %d", len(want), len(harnesses))
	}
	for _, test := range want {
		request := testRequest(t, "bedrock", string(test.provider), task, "work")
		if got := launchCommand(request, ref); got != test.command {
			t.Errorf("launchCommand() for %s =\n%s\nwant\n%s", test.provider, got, test.command)
		}
	}
}

// TestLaunchCommandReachesTheHarnessUnchanged runs each provider's command through
// a real shell twice, the way ssh and tmux would, and asserts the harness receives
// one prompt argument holding the packet's absolute path and the task byte for
// byte. Every other test here reads the command as text; this one is the proof
// that the quoting survives both shells, and that the session writes the marker
// devbox later reports.
func TestLaunchCommandReachesTheHarnessUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the box runs a POSIX shell; this test needs one on the operator's machine too")
	}
	const ref = "0123456789abcdef"
	task := "fix it's \"$HOME\" and $(whoami) and `id` now"

	root := t.TempDir()
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	inner := filepath.Join(root, "inner")
	argv := filepath.Join(root, "argv")
	session := filepath.Join(home, box.AgentRoot, ref)
	for _, dir := range []string{bin, filepath.Join(home, box.TreeRoot, "work"), session} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeStub(t, filepath.Join(bin, "tmux"), "#!/bin/sh\nlast=\nfor arg in \"$@\"; do last=\"$arg\"; done\nprintf '%s' \"$last\" > \"$DEVBOX_STUB_INNER\"\n")

	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("DEVBOX_STUB_INNER", inner)
	t.Setenv("DEVBOX_STUB_ARGV", argv)

	for _, provider := range harnesses {
		t.Run(provider.binary, func(t *testing.T) {
			writeStub(t, filepath.Join(bin, provider.binary), "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DEVBOX_STUB_ARGV\"\n")
			request := testRequest(t, "bedrock", string(provider.provider), task, "work")
			run(t, "/bin/sh", "-c", launchCommand(request, ref))
			tmuxSaw := readFile(t, inner)
			run(t, "/bin/sh", "-c", tmuxSaw)

			prompt := "Read the handoff packet at " + filepath.Join(session, handoffFile) + " and work the task it describes: " + task
			want := append(append([]string{}, provider.flags...), prompt)
			if got := strings.Split(strings.TrimSuffix(readFile(t, argv), "\n"), "\n"); !equal(got, want) {
				t.Fatalf("the harness received %q, want %q", got, want)
			}
			if got := readFile(t, filepath.Join(session, statusFile)); got != "done\n" {
				t.Fatalf("the session wrote %q to the status file, want \"done\\n\"", got)
			}
		})
	}
}

// TestRemoteDirExprRefusesWhatItCannotQuote covers the directions a working
// directory can be unusable: not a path at all, and a path a shell would expand
// inside the double quotes devbox has to use for the remote home.
func TestRemoteDirExprRefusesWhatItCannotQuote(t *testing.T) {
	for _, dir := range []string{"relative/work", "/mnt/data/$(whoami)", "/mnt/data/dir\"name", "/mnt/data/dir\nnext"} {
		if _, err := remoteDirExpr(dir); err == nil {
			t.Errorf("remoteDirExpr(%q) was accepted, want a refusal", dir)
		}
	}
	got, err := remoteDirExpr("~/src/dev box")
	if err != nil {
		t.Fatalf("remoteDirExpr(~/src/dev box) error = %v", err)
	}
	if want := `"$HOME/src/dev box"`; got != want {
		t.Fatalf("remoteDirExpr(~/src/dev box) = %s, want %s", got, want)
	}
}

// TestParseSessionRefRefusesTmuxTargetSeparators keeps an id from addressing a
// session other than the one the operator typed: tmux reads a colon and a period
// as separators inside a target.
func TestParseSessionRefRefusesTmuxTargetSeparators(t *testing.T) {
	for _, ref := range []string{"", "abc:def", "abc.def", "abc def", "abc,def"} {
		if _, err := ParseSessionRef(ref); err == nil {
			t.Errorf("ParseSessionRef(%q) was accepted, want a refusal", ref)
		}
	}
	id, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID() error = %v", err)
	}
	if _, err := ParseSessionRef(id); err != nil {
		t.Fatalf("newSessionID() returned %q, which ParseSessionRef rejects: %v", id, err)
	}
}

// writeStub installs an executable a test shell can call by name.
func writeStub(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// readFile reads a file one stub wrote.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// run executes one shell command and fails the test if it did not succeed.
func run(t *testing.T, argv ...string) {
	t.Helper()
	if output, err := exec.CommandContext(context.Background(), argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, output)
	}
}

// equal compares two argument vectors, so a test reports a wrong argument rather
// than a wrong length.
func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
