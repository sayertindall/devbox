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

// TestLaunchCommandQuotesTheTaskForEveryShell pins the exact bytes devbox sends
// for a task holding a single quote, a double quote, and a dollar sign. The task
// crosses the login shell ssh starts and the shell tmux runs the session with, so
// the string is the contract: it is quoted once for tmux and once for each shell.
// The round trip below proves the bytes mean what they say.
func TestLaunchCommandQuotesTheTaskForEveryShell(t *testing.T) {
	request, err := newRequest(box.Name("bedrock"), "omp", `fix it's "$HOME" now`, "work", "")
	if err != nil {
		t.Fatalf("newRequest() error = %v", err)
	}
	const ref = "0123456789abcdef"

	want := `tmux new-session -d -s devbox-0123456789abcdef ` +
		`'cd "$HOME/devbox/trees/work" && omp --task-file "$HOME/devbox/agents/0123456789abcdef/handoff.json" ` +
		`'\''fix it'\''\'\'''\''s "$HOME" now'\''` +
		`; echo done > "$HOME/devbox/agents/0123456789abcdef/status"'`

	if got := launchCommand(request, ref); got != want {
		t.Fatalf("launchCommand() =\n%s\nwant\n%s", got, want)
	}
}

// TestLaunchCommandReachesTheHarnessUnchanged runs the command devbox builds
// through a real shell twice, the way ssh and tmux would, and asserts the harness
// receives the task byte for byte. Every other test here reads the command as
// text; this one is the proof that the quoting survives both shells, and that the
// session writes the marker devbox later reports.
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
	work := filepath.Join(home, box.TreeRoot, "work")
	session := filepath.Join(home, box.AgentRoot, ref)
	for _, dir := range []string{bin, work, session} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeStub(t, filepath.Join(bin, "tmux"), "#!/bin/sh\nlast=\nfor arg in \"$@\"; do last=\"$arg\"; done\nprintf '%s' \"$last\" > \"$DEVBOX_STUB_INNER\"\n")
	writeStub(t, filepath.Join(bin, "omp"), "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DEVBOX_STUB_ARGV\"\n")

	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("DEVBOX_STUB_INNER", inner)
	t.Setenv("DEVBOX_STUB_ARGV", argv)

	request, err := newRequest(box.Name("bedrock"), "omp", task, "work", "")
	if err != nil {
		t.Fatalf("newRequest() error = %v", err)
	}
	run(t, "/bin/sh", "-c", launchCommand(request, ref))
	tmuxSaw := readFile(t, inner)
	run(t, "/bin/sh", "-c", tmuxSaw)

	if got := strings.Split(strings.TrimSuffix(readFile(t, argv), "\n"), "\n"); len(got) != 3 ||
		got[0] != "--task-file" ||
		got[1] != filepath.Join(session, handoffFile) ||
		got[2] != task {
		t.Fatalf("the harness received %q, want --task-file %s and the task unchanged", got, filepath.Join(session, handoffFile))
	}
	if got := readFile(t, filepath.Join(session, statusFile)); got != "done\n" {
		t.Fatalf("the session wrote %q to the status file, want \"done\\n\"", got)
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
