package access

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"devbox/internal/box"
)

// homeWithSSH points the SSH configuration lookup at a temporary home, so no test
// touches the operator's own file.
func homeWithSSH(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path, err := sshConfigPath()
	if err != nil {
		t.Fatalf("resolve the ssh configuration path: %v", err)
	}
	return path
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestBlockTunnelsABoxThatHasNoExternalAddress(t *testing.T) {
	path := homeWithSSH(t)
	block, err := writeSSHBlock(path, testConfig(), "alpha", "")
	if err != nil {
		t.Fatalf("write the block: %v", err)
	}
	want := []string{
		"Host devbox-alpha",
		"  HostName alpha",
		"  ProxyCommand gcloud compute start-iap-tunnel %h %p --listen-on-stdin --project=example-project --zone=us-central1-a --verbosity=warning",
		"  User builder",
		"  StrictHostKeyChecking accept-new",
		"  ServerAliveInterval 30",
	}
	if !slices.Equal(block.Lines, want) {
		t.Errorf("entry =\n%s\nwant\n%s", strings.Join(block.Lines, "\n"), strings.Join(want, "\n"))
	}
	raw := read(t, path)
	for _, line := range want {
		if !strings.Contains(raw, line+"\n") {
			t.Errorf("%s is missing from the file:\n%s", line, raw)
		}
	}
	if !strings.HasPrefix(raw, box.SSHConfigMarkerStart+"\n") || !strings.HasSuffix(raw, box.SSHConfigMarkerEnd+"\n") {
		t.Errorf("the entry is not delimited by the managed markers:\n%s", raw)
	}
}

func TestBlockReachesABoxThatHasAnExternalAddressDirectly(t *testing.T) {
	path := homeWithSSH(t)
	block, err := writeSSHBlock(path, testConfig(), "alpha", "34.5.6.7")
	if err != nil {
		t.Fatalf("write the block: %v", err)
	}
	want := []string{
		"Host devbox-alpha",
		"  HostName 34.5.6.7",
		"  User builder",
		"  StrictHostKeyChecking accept-new",
		"  ServerAliveInterval 30",
	}
	if !slices.Equal(block.Lines, want) {
		t.Errorf("entry =\n%s\nwant\n%s", strings.Join(block.Lines, "\n"), strings.Join(want, "\n"))
	}
	if raw := read(t, path); strings.Contains(raw, "ProxyCommand") {
		t.Errorf("a box with an address needs no tunnel:\n%s", raw)
	}
}

func TestBlockNamesTheIdentityFileWhenOneIsConfigured(t *testing.T) {
	path := homeWithSSH(t)
	cfg := testConfig()
	cfg.SSHKey = "~/.ssh/devbox_ed25519"
	block, err := writeSSHBlock(path, cfg, "alpha", "34.5.6.7")
	if err != nil {
		t.Fatalf("write the block: %v", err)
	}
	if !slices.Contains(block.Lines, "  IdentityFile ~/.ssh/devbox_ed25519") {
		t.Errorf("entry =\n%s\nwant the configured identity file", strings.Join(block.Lines, "\n"))
	}
}

func TestWritingTheSameBlockTwiceLeavesTheFileIdentical(t *testing.T) {
	path := homeWithSSH(t)
	if _, err := writeSSHBlock(path, testConfig(), "alpha", ""); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first := read(t, path)
	block, err := writeSSHBlock(path, testConfig(), "alpha", "")
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if second := read(t, path); second != first {
		t.Errorf("the file changed on the second run:\n%s\n---\n%s", first, second)
	}
	if block.Changed {
		t.Error("the second run reported a change it did not make")
	}
}

func TestWritingTheBlockKeepsWhatTheOperatorWrote(t *testing.T) {
	path := homeWithSSH(t)
	above := "Host bastion\n  HostName bastion.example.com\n  User ops\n\n"
	below := "\n# keep this comment\nHost retired\n  Port 2222\n"
	original := above + box.SSHConfigMarkerStart + "\n" +
		"Host devbox-alpha\n  HostName 10.0.0.9\n  User stale\n" +
		box.SSHConfigMarkerEnd + "\n" + below
	write(t, path, original)

	if _, err := writeSSHBlock(path, testConfig(), "alpha", "34.5.6.7"); err != nil {
		t.Fatalf("write the block: %v", err)
	}
	raw := read(t, path)
	if !strings.HasPrefix(raw, above) {
		t.Errorf("the configuration above the block changed:\n%s", raw)
	}
	if !strings.HasSuffix(raw, below) {
		t.Errorf("the configuration below the block changed:\n%s", raw)
	}
	if strings.Contains(raw, "10.0.0.9") || strings.Contains(raw, "User stale") {
		t.Errorf("the previous entry for this box survived:\n%s", raw)
	}
	if !strings.Contains(raw, "  HostName 34.5.6.7\n") {
		t.Errorf("the new address is missing:\n%s", raw)
	}
}

func TestWritingOneBoxKeepsTheEntriesOfTheOthers(t *testing.T) {
	path := homeWithSSH(t)
	if _, err := writeSSHBlock(path, testConfig(), "beta", "34.5.6.7"); err != nil {
		t.Fatalf("write beta: %v", err)
	}
	if _, err := writeSSHBlock(path, testConfig(), "alpha", ""); err != nil {
		t.Fatalf("write alpha: %v", err)
	}
	if _, err := writeSSHBlock(path, testConfig(), "alpha", "10.1.1.1"); err != nil {
		t.Fatalf("rewrite alpha: %v", err)
	}
	raw := read(t, path)
	beta := "Host devbox-beta\n  HostName 34.5.6.7\n  User builder\n  StrictHostKeyChecking accept-new\n  ServerAliveInterval 30\n"
	if !strings.Contains(raw, beta) {
		t.Errorf("rewriting one box changed another:\n%s", raw)
	}
	if strings.Index(raw, "Host devbox-alpha") > strings.Index(raw, "Host devbox-beta") {
		t.Errorf("entries are not written in alias order:\n%s", raw)
	}
	if _, err := writeSSHBlock(path, testConfig(), "alpha", "10.1.1.1"); err != nil {
		t.Fatalf("rewrite alpha again: %v", err)
	}
	if again := read(t, path); again != raw {
		t.Errorf("rewriting with the same address changed the file:\n%s\n---\n%s", raw, again)
	}
}

func TestBlockIsRefusedWhenTheMarkersDoNotPair(t *testing.T) {
	start := box.SSHConfigMarkerStart
	end := box.SSHConfigMarkerEnd
	cases := []struct {
		what    string
		content string
	}{
		{"a repeated start marker", start + "\nHost a\n" + start + "\n" + end + "\n"},
		{"a repeated end marker", start + "\nHost a\n" + end + "\n" + end + "\n"},
		{"a start marker with no end", "Host a\n" + start + "\n"},
		{"an end marker with no start", "Host a\n" + end + "\n"},
		{"an end marker before its start", end + "\nHost a\n" + start + "\n"},
		{"text inside the block that is not an entry", start + "\njust a note\n" + end + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			path := homeWithSSH(t)
			write(t, path, tc.content)
			if _, err := writeSSHBlock(path, testConfig(), "alpha", ""); err == nil {
				t.Fatal("a file devbox cannot rewrite faithfully must be refused")
			}
			if raw := read(t, path); raw != tc.content {
				t.Errorf("the file changed despite the refusal:\n%s", raw)
			}
		})
	}
}

func TestBlockCreatesTheFilePrivateAndKeepsItPrivate(t *testing.T) {
	path := homeWithSSH(t)
	if _, err := writeSSHBlock(path, testConfig(), "alpha", ""); err != nil {
		t.Fatalf("write the block: %v", err)
	}
	assertMode(t, filepath.Dir(path), 0o700)
	assertMode(t, path, 0o600)

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	block, err := writeSSHBlock(path, testConfig(), "alpha", "")
	if err != nil {
		t.Fatalf("rewrite the block: %v", err)
	}
	if block.Changed {
		t.Error("a run with the same content reported a change")
	}
	assertMode(t, path, 0o600)
}

func TestBlockFollowsASymlinkedConfiguration(t *testing.T) {
	path := homeWithSSH(t)
	real := filepath.Join(t.TempDir(), "config")
	write(t, real, "Host bastion\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSSHBlock(path, testConfig(), "alpha", ""); err != nil {
		t.Fatalf("write the block: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the operator's symbolic link was replaced by a regular file")
	}
	raw := read(t, real)
	if !strings.HasPrefix(raw, "Host bastion\n") || !strings.Contains(raw, "Host devbox-alpha\n") {
		t.Errorf("the entry did not reach the file the link points at:\n%s", raw)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s has mode %04o, want %04o", path, got, want)
	}
}

// TestEntryNamesTheKeyOSLoginAccepts covers the key that a box with OS Login
// enabled will actually accept: the one gcloud registered, unless the
// configuration names another.
func TestEntryNamesTheKeyOSLoginAccepts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := filepath.Join(home, ".ssh", "google_compute_engine")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	lines := hostStanza(cfg, box.Name("alpha"), "").lines
	if !slices.Contains(lines, "  IdentityFile "+key) {
		t.Fatalf("the entry does not name the OS Login key:\n%s", strings.Join(lines, "\n"))
	}
	cfg.SSHKey = "/keys/mine"
	if named := hostStanza(cfg, box.Name("alpha"), "").lines; !slices.Contains(named, "  IdentityFile /keys/mine") {
		t.Fatalf("a named key must win over the default:\n%s", strings.Join(named, "\n"))
	}
}

// TestEntryNamesNoKeyWhenThereIsNone keeps the entry honest: a path that does not
// exist would make ssh fail with a message about a missing file instead of about
// the key it cannot use.
func TestEntryNamesNoKeyWhenThereIsNone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, line := range hostStanza(testConfig(), box.Name("alpha"), "").lines {
		if strings.HasPrefix(line, "  IdentityFile") {
			t.Fatalf("the entry names a key that does not exist: %s", line)
		}
	}
}
