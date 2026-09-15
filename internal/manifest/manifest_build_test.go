// Build-path tests: what the source walk includes, excludes, and refuses.
// The long directory-replacement race harness lives in manifest_race_test.go.
package manifest

import (
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func TestBuildIncludesDirtyAndUntrackedAllowedFiles(t *testing.T) {
	root := t.TempDir()
	// Committed-and-then-modified, plus never-tracked, plus a staged-only path.
	write(t, root, "README.md", "modified after commit\n", 0o644)
	write(t, root, "main.go", "package main\n", 0o644)
	write(t, root, "pkg/untracked.go", "package pkg\n", 0o644)
	write(t, root, "scripts/run.sh", "#!/bin/sh\necho hi\n", 0o755)
	symlink(t, root, "link.go", "pkg/untracked.go")
	// Git storage is never source.
	write(t, root, ".git/config", "[core]\n", 0o644)

	m := mustBuild(t, root)

	want := []string{"README.md", "link.go", "main.go", "pkg/untracked.go", "scripts/run.sh"}
	got := paths(m)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	if m.Version != Version {
		t.Fatalf("version = %d, want %d", m.Version, Version)
	}

	readme := find(t, m, "README.md")
	if readme.Kind != KindFile || readme.Executable {
		t.Fatalf("README.md entry = %+v, want non-executable file", readme)
	}
	if readme.Size != int64(len("modified after commit\n")) {
		t.Fatalf("README.md size = %d", readme.Size)
	}
	if readme.SHA256 != digest("modified after commit\n") {
		t.Fatalf("README.md sha256 = %s, want %s", readme.SHA256, digest("modified after commit\n"))
	}
	if readme.Target != "" {
		t.Fatalf("README.md target = %q, want empty", readme.Target)
	}

	if script := find(t, m, "scripts/run.sh"); !script.Executable {
		t.Fatalf("scripts/run.sh entry = %+v, want executable", script)
	}

	link := find(t, m, "link.go")
	if link.Kind != KindSymlink || link.Target != "pkg/untracked.go" {
		t.Fatalf("link.go entry = %+v, want symlink to pkg/untracked.go", link)
	}
	if link.Size != 0 || link.SHA256 != "" {
		t.Fatalf("link.go entry = %+v, want no size or digest", link)
	}

	var wantBytes int64
	for _, content := range []string{"modified after commit\n", "package main\n", "package pkg\n", "#!/bin/sh\necho hi\n"} {
		wantBytes += int64(len(content))
	}
	if m.Bytes != wantBytes {
		t.Fatalf("bytes = %d, want %d", m.Bytes, wantBytes)
	}
}

func TestBuildExcludesTrackedSecretPath(t *testing.T) {
	root := t.TempDir()
	excluded := []string{
		".env",
		".env.local",
		".env.production",
		".ssh/id_ed25519",
		".aws/credentials",
		".config/gh/hosts.yml",
		".claude/settings.json",
		".codex/auth.json",
		".omp/state.json",
		".agentbox/project.toml",
		".git/config",
		"node_modules/pkg/index.js",
		"dist/app.js",
		"build/app.o",
		"sub/node_modules/nested/index.js",
		"sub/.env",
	}
	for _, rel := range excluded {
		write(t, root, rel, "canary\n", 0o600)
	}
	write(t, root, "app.go", "package app\n", 0o644)

	m := mustBuild(t, root)

	if got := paths(m); len(got) != 1 || got[0] != "app.go" {
		t.Fatalf("entries = %v, want only app.go", got)
	}
	digestOfCanary := digest("canary\n")
	for _, e := range m.Entries {
		if e.SHA256 == digestOfCanary {
			t.Fatalf("excluded canary content leaked through %q", e.Path)
		}
	}

	// A symlink may not reach into an excluded path either.
	symlink(t, root, "leak", ".env")
	if _, err := Build(root, Policy{}); err == nil {
		t.Fatal("Build accepted a symlink into an excluded path")
	}
}

func TestBuildRejectsCaseCollision(t *testing.T) {
	root := t.TempDir()
	write(t, root, "README.md", "upper\n", 0o644)

	if caseSensitive(t, root) {
		write(t, root, "readme.md", "lower\n", 0o644)
		_, err := Build(root, Policy{})
		if err == nil {
			t.Fatal("Build accepted a case-insensitive path collision")
		}
		if !strings.Contains(err.Error(), "case") {
			t.Fatalf("error = %v, want a case-collision error", err)
		}
		return
	}

	// The host filesystem is case-insensitive, so the collision cannot exist on
	// disk. Exercise the identical rule Build applies, through the public API a
	// remote manifest arrives on.
	t.Log("case-insensitive host filesystem: validating the collision rule through Validate and Materialize")
	m := mustBuild(t, root)
	m.Entries = append(m.Entries, Entry{
		Path:   "readme.md",
		Kind:   KindFile,
		Size:   m.Entries[0].Size,
		SHA256: m.Entries[0].SHA256,
	})
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })

	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "case") {
		t.Fatalf("Validate error = %v, want a case-collision error", err)
	}
	if err := Materialize(root, m, openDest(t, filepath.Join(t.TempDir(), "dst"))); err == nil {
		t.Fatal("Materialize accepted a case-insensitive path collision")
	}
}

func TestBuildRejectsAbsoluteAndOutsideSymlink(t *testing.T) {
	t.Run("absolute", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "app.go", "package app\n", 0o644)
		symlink(t, root, "secret", "/etc/passwd")
		if _, err := Build(root, Policy{}); err == nil {
			t.Fatal("Build accepted an absolute symlink")
		}
	})

	t.Run("outside root", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.WriteFile(filepath.Join(parent, "outside.txt"), []byte("outside\n"), 0o644); err != nil {
			t.Fatalf("write outside file: %v", err)
		}
		root := filepath.Join(parent, "project")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatalf("mkdir project: %v", err)
		}
		write(t, root, "app.go", "package app\n", 0o644)
		symlink(t, root, "escape", "../outside.txt")
		if _, err := Build(root, Policy{}); err == nil {
			t.Fatal("Build accepted a symlink resolving outside root")
		}
	})

	t.Run("outside root through subdirectory", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "project")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatalf("mkdir project: %v", err)
		}
		write(t, root, "sub/app.go", "package app\n", 0o644)
		symlink(t, root, "sub/escape", "../../outside.txt")
		if _, err := Build(root, Policy{}); err == nil {
			t.Fatal("Build accepted a nested symlink resolving outside root")
		}
	})

	t.Run("relative inside root is allowed", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "sub/app.go", "package app\n", 0o644)
		symlink(t, root, "sub/alias.go", "app.go")
		m := mustBuild(t, root)
		if e := find(t, m, "sub/alias.go"); e.Target != "app.go" {
			t.Fatalf("entry = %+v, want target app.go", e)
		}
	})

	t.Run("relative symlink to a declared directory is allowed", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "sub/app.go", "package app\n", 0o644)
		symlink(t, root, "dirlink", "sub")
		symlink(t, root, "deep/up", "../sub/app.go")
		m := mustBuild(t, root)
		if e := find(t, m, "dirlink"); e.Target != "sub" {
			t.Fatalf("entry = %+v, want target sub", e)
		}
		if e := find(t, m, "deep/up"); e.Target != "../sub/app.go" {
			t.Fatalf("entry = %+v, want target ../sub/app.go", e)
		}
	})
}

func TestBuildRejectsFIFOOrSocket(t *testing.T) {
	t.Run("fifo", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "app.go", "package app\n", 0o644)
		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
		if _, err := Build(root, Policy{}); err == nil {
			t.Fatal("Build accepted a FIFO")
		}
	})

	t.Run("socket", func(t *testing.T) {
		// A unix socket path must fit in sun_path (104 bytes on darwin), which
		// the default test temp directory does not, so use a short root.
		root, err := os.MkdirTemp("/tmp", "abx")
		if err != nil {
			t.Fatalf("temp root: %v", err)
		}
		t.Cleanup(func() { os.RemoveAll(root) })
		write(t, root, "app.go", "package app\n", 0o644)
		listener, err := net.Listen("unix", filepath.Join(root, "s.sock"))
		if err != nil {
			t.Fatalf("listen unix: %v", err)
		}
		defer listener.Close()
		if _, err := Build(root, Policy{}); err == nil {
			t.Fatal("Build accepted a unix socket")
		}
	})
}

func TestBuildRejectsInvalidUTF8Path(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app.go", "package app\n", 0o644)

	invalid := "bad\xff\xfe.txt"
	if err := os.WriteFile(filepath.Join(root, invalid), []byte("x"), 0o644); err == nil {
		_, buildErr := Build(root, Policy{})
		if buildErr == nil {
			t.Fatal("Build accepted an invalid UTF-8 path")
		}
		if !strings.Contains(buildErr.Error(), "UTF-8") {
			t.Fatalf("error = %v, want a UTF-8 error", buildErr)
		}
		return
	}

	// This filesystem refuses to create such a name, but a manifest arriving
	// from a server filesystem that permits arbitrary bytes must still be
	// rejected, and it must be rejected before it reaches JSON encoding.
	t.Log("host filesystem refuses invalid UTF-8 names: pinning the rule on Validate, Encode, and Materialize")

	badPath := Manifest{Version: Version, Entries: []Entry{{
		Path:   invalid,
		Kind:   KindFile,
		Size:   1,
		SHA256: digest("x"),
	}}}
	err := badPath.Validate()
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("Validate error = %v, want a UTF-8 error", err)
	}
	if _, err := Encode(badPath); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("Encode error = %v, want a UTF-8 error before JSON encoding", err)
	}
	if err := Materialize(root, badPath, openDest(t, filepath.Join(t.TempDir(), "dst"))); err == nil {
		t.Fatal("Materialize accepted an invalid UTF-8 path")
	}

	badTarget := Manifest{Version: Version, Entries: []Entry{{
		Path:   "link",
		Kind:   KindSymlink,
		Target: invalid,
	}}}
	if err := badTarget.Validate(); err == nil {
		t.Fatal("Validate accepted an invalid UTF-8 symlink target")
	}
	if _, err := Encode(badTarget); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("Encode error = %v, want a UTF-8 error before JSON encoding", err)
	}
}

func TestBuildRejectsDanglingSymlink(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
	}{
		{"missing sibling", "missing.txt"},
		{"missing inside existing directory", "sub/missing.txt"},
		{"missing directory", "nosuchdir/file.txt"},
		{"descend through a declared file", "sub/keep.txt/deeper.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "sub/keep.txt", "keep\n", 0o644)
			symlink(t, root, "dangling", tc.target)
			_, err := Build(root, Policy{})
			if err == nil {
				t.Fatalf("Build accepted a dangling symlink to %q", tc.target)
			}
			if !strings.Contains(err.Error(), "dangling") && !strings.Contains(err.Error(), "resolve") {
				t.Fatalf("error = %v, want a dangling-target error", err)
			}
		})
	}

	t.Run("target declared through another symlink is allowed", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "sub/keep.txt", "keep\n", 0o644)
		symlink(t, root, "dirlink", "sub")
		symlink(t, root, "filelink", "dirlink/keep.txt")
		m := mustBuild(t, root)
		if e := find(t, m, "filelink"); e.Target != "dirlink/keep.txt" {
			t.Fatalf("entry = %+v, want the chained symlink preserved", e)
		}
	})
}

func TestBuildRejectsSymlinkCycle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links [][2]string
	}{
		{"self reference", [][2]string{{"loop", "loop"}}},
		{"two node cycle", [][2]string{{"a", "b"}, {"b", "a"}}},
		{"three node cycle", [][2]string{{"x", "y"}, {"y", "z"}, {"z", "x"}}},
		{"cycle through a directory component", [][2]string{{"d", "e/keep.txt"}, {"e", "d"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "app.go", "package app\n", 0o644)
			for _, link := range tc.links {
				symlink(t, root, link[0], link[1])
			}
			_, err := Build(root, Policy{})
			if err == nil {
				t.Fatalf("Build accepted symlink cycle %v", tc.links)
			}
			if !strings.Contains(err.Error(), "cycle") {
				t.Fatalf("error = %v, want a cycle error", err)
			}
		})
	}
}

func TestBuildRejectsSymlinkTargetEscapingThroughInternalSymlink(t *testing.T) {
	// Lexical cleanup of the raw target lands inside the root, but physical
	// resolution walks out through an internal symlink component.
	grand := t.TempDir()
	const secret = "OUTSIDE SECRET\n"
	if err := os.WriteFile(filepath.Join(grand, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}
	root := filepath.Join(grand, "mid", "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	write(t, root, "a/b/keep.txt", "keep\n", 0o644)
	// A declared file at the path the raw target lexically cleans to, so the
	// escape cannot be mistaken for a merely dangling target.
	write(t, root, "a/secret.txt", "inside\n", 0o644)
	// tosub resolves to the ordinary directory "a", so nothing about this link
	// is suspicious on its own and lexical cleanup of the trap lands on a real
	// declared file. Only physical resolution shows the escape.
	symlink(t, root, "a/b/tosub", "..")
	const trap = "a/b/tosub/../../secret.txt"
	symlink(t, root, "evil", trap)

	if lexical := path.Clean(trap); lexical != "a/secret.txt" {
		t.Fatalf("test premise broken: lexical cleanup of %q is %q", trap, lexical)
	}

	m, err := Build(root, Policy{})
	if err == nil {
		t.Fatalf("Build accepted a target escaping through an internal symlink; entries %v", paths(m))
	}
	if !strings.Contains(err.Error(), "outside root") {
		t.Fatalf("error = %v, want an outside-root error", err)
	}
	if strings.Contains(err.Error(), "a/b/tosub") && !strings.Contains(err.Error(), "evil") {
		t.Fatalf("error blames the innocent link instead of the trap: %v", err)
	}

	// Removing only the trap must leave the tree valid, so the internal symlink
	// to a declared directory keeps working.
	if err := os.Remove(filepath.Join(root, "evil")); err != nil {
		t.Fatalf("remove trap: %v", err)
	}
	clean, err := Build(root, Policy{})
	if err != nil {
		t.Fatalf("Build rejected the tree after the trap was removed: %v", err)
	}
	if e := find(t, clean, "a/b/tosub"); e.Target != ".." {
		t.Fatalf("entry = %+v, want the internal directory symlink preserved", e)
	}
}

func TestBuildRejectsDirectoryReplacementWithOutsideSymlink(t *testing.T) {
	const secret = "OUTSIDE SECRET\n"
	secretDigest := digest(secret)

	setup := func(t *testing.T) (root, outside string) {
		t.Helper()
		parent := t.TempDir()
		outside = filepath.Join(parent, "outside")
		if err := os.Mkdir(outside, 0o755); err != nil {
			t.Fatalf("mkdir outside: %v", err)
		}
		if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret), 0o600); err != nil {
			t.Fatalf("write outside secret: %v", err)
		}
		root = filepath.Join(parent, "project")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatalf("mkdir project: %v", err)
		}
		write(t, root, "app.go", "package app\n", 0o644)
		return root, outside
	}

	t.Run("relative outside symlink in place of a directory", func(t *testing.T) {
		root, _ := setup(t)
		symlink(t, root, "sub", "../outside")
		m, err := Build(root, Policy{})
		if err == nil {
			t.Fatalf("Build accepted a directory replaced by an outside symlink; entries %v", paths(m))
		}
	})

	t.Run("absolute outside symlink in place of a directory", func(t *testing.T) {
		root, outside := setup(t)
		symlink(t, root, "sub", outside)
		m, err := Build(root, Policy{})
		if err == nil {
			t.Fatalf("Build accepted an absolute outside symlink; entries %v", paths(m))
		}
		if m.SHA256 != "" {
			t.Fatalf("failed Build returned a manifest: %+v", m)
		}
	})

	t.Run("concurrent directory replacement", func(t *testing.T) {
		root, outside := setup(t)
		runDirectoryReplacementRace(t, root, outside, secretDigest)
	})
}

func TestManifestDigestIsDeterministic(t *testing.T) {
	build := func(root string) Manifest {
		write(t, root, "b.txt", "bbb\n", 0o644)
		write(t, root, "a/deep.txt", "deep\n", 0o644)
		write(t, root, "a.txt", "aaa\n", 0o755)
		symlink(t, root, "link", "b.txt")
		write(t, root, ".env", "SECRET=1\n", 0o600)
		return mustBuild(t, root)
	}

	first := build(t.TempDir())
	second := build(t.TempDir())

	if first.SHA256 != second.SHA256 {
		t.Fatalf("digest differs across identical trees: %s vs %s", first.SHA256, second.SHA256)
	}
	if strings.Join(paths(first), ",") != strings.Join(paths(second), ",") {
		t.Fatalf("ordering differs: %v vs %v", paths(first), paths(second))
	}
	if !sort.SliceIsSorted(first.Entries, func(i, j int) bool { return first.Entries[i].Path < first.Entries[j].Path }) {
		t.Fatalf("entries are not sorted: %v", paths(first))
	}

	encoded, err := Encode(first)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if want := digest(string(encoded)); first.SHA256 != want {
		t.Fatalf("digest = %s, want sha256 of canonical encoding %s", first.SHA256, want)
	}
	againEncoded, err := Encode(second)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(encoded) != string(againEncoded) {
		t.Fatalf("canonical encoding is not byte-identical")
	}

	// Content change moves the digest.
	changedRoot := t.TempDir()
	changed := build(changedRoot)
	write(t, changedRoot, "b.txt", "bbbb\n", 0o644)
	after := mustBuild(t, changedRoot)
	if after.SHA256 == changed.SHA256 {
		t.Fatal("digest did not change after content change")
	}

	if err := Compare(first, second); err != nil {
		t.Fatalf("Compare of identical manifests: %v", err)
	}
	if err := Compare(first, after); err == nil {
		t.Fatal("Compare accepted mismatched manifests")
	}
}

func TestBuildRejectsSymlinkTraversalThroughRegularFile(t *testing.T) {
	// A real filesystem answers ENOTDIR for any component after a regular file,
	// including "." and "..", so the manifest graph must answer the same way.
	for _, target := range []string{
		"link/..",
		"file/..",
		"file/.",
		"file/",
		"file/inner.txt",
		"chain/..",
		"chain/./..",
		"link/../file",
	} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "file", "content\n", 0o644)
			symlink(t, root, "link", "file")
			symlink(t, root, "chain", "link")
			symlink(t, root, "traversal", target)

			m, err := Build(root, Policy{})
			if err == nil {
				t.Fatalf("Build accepted symlink target %q traversing through a regular file; entries %v", target, paths(m))
			}
			if !strings.Contains(err.Error(), "traversal") {
				t.Fatalf("error = %v, want it to name the offending link", err)
			}
		})
	}

	t.Run("a symlink chain ending at a file is still allowed", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "file", "content\n", 0o644)
		write(t, root, "sub/deep.txt", "deep\n", 0o644)
		symlink(t, root, "link", "file")
		symlink(t, root, "chain", "link")
		symlink(t, root, "intodir", "sub")
		symlink(t, root, "throughdir", "intodir/deep.txt")
		symlink(t, root, "dirparent", "sub/..")
		m := mustBuild(t, root)
		for _, want := range []string{"chain", "intodir", "link", "throughdir", "dirparent"} {
			find(t, m, want)
		}
	})
}

func TestBuildRejectsNestedGitDirectory(t *testing.T) {
	root := t.TempDir()
	// Repository storage at the root is ordinary and stays excluded.
	write(t, root, ".git/config", "[core]\n", 0o644)
	write(t, root, ".git/objects/pack/pack-1.idx", "binary\n", 0o644)
	write(t, root, "app.go", "package app\n", 0o644)
	write(t, root, "vendor/dep/main.go", "package dep\n", 0o644)
	write(t, root, "vendor/dep/.git/config", "[core]\n", 0o644)

	m, err := Build(root, Policy{})
	if err == nil {
		t.Fatalf("Build accepted a nested repository; entries %v", paths(m))
	}
	if !strings.Contains(err.Error(), "vendor/dep") {
		t.Fatalf("error = %v, want it to name the nested repository", err)
	}
	if m.SHA256 != "" || len(m.Entries) != 0 {
		t.Fatalf("failed Build returned a partial manifest: %+v", m)
	}

	// Removing the nested repository leaves an ordinary project with root
	// storage still excluded.
	if err := os.RemoveAll(filepath.Join(root, "vendor", "dep", ".git")); err != nil {
		t.Fatalf("remove nested git: %v", err)
	}
	clean := mustBuild(t, root)
	if got := paths(clean); strings.Join(got, ",") != "app.go,vendor/dep/main.go" {
		t.Fatalf("entries = %v, want app.go and vendor/dep/main.go", got)
	}
}

func TestBuildRejectsNestedGitFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string
	}{
		{"submodule marker one level down", "sub/.git"},
		{"submodule marker deep down", "a/b/c/.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ".git/config", "[core]\n", 0o644)
			write(t, root, "app.go", "package app\n", 0o644)
			write(t, root, path.Dir(tc.rel)+"/main.go", "package sub\n", 0o644)
			// A submodule records its real Git directory in a .git *file*.
			write(t, root, tc.rel, "gitdir: ../.git/modules/sub\n", 0o644)

			m, err := Build(root, Policy{})
			if err == nil {
				t.Fatalf("Build accepted a submodule marker file; entries %v", paths(m))
			}
			if !strings.Contains(err.Error(), path.Dir(tc.rel)) {
				t.Fatalf("error = %v, want it to name %q", err, path.Dir(tc.rel))
			}
			for _, e := range m.Entries {
				if strings.Contains(e.Path, path.Dir(tc.rel)) {
					t.Fatalf("nested repository path %q was still declared", e.Path)
				}
			}
		})
	}

	t.Run("a root .git file is repository storage, not a nested repository", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "app.go", "package app\n", 0o644)
		// A worktree checkout records its Git directory in a root .git file.
		write(t, root, ".git", "gitdir: /elsewhere/.git/worktrees/wt\n", 0o644)
		m := mustBuild(t, root)
		if got := paths(m); len(got) != 1 || got[0] != "app.go" {
			t.Fatalf("entries = %v, want only app.go", got)
		}
	})
}

// caseVariantSecrets are real secret and build paths spelled in a case the
// exclusion list does not literally contain. On a case-insensitive filesystem
// each one names exactly the same file as its lowercase form.
var caseVariantSecrets = []string{
	".ENV",
	".Env.local",
	".ENV.PRODUCTION",
	".SSH/id_ed25519",
	".AWS/credentials",
	".Config/gh/hosts.yml",
	".CLAUDE/settings.json",
	".Codex/auth.json",
	".OMP/state.json",
	".AgentBox/project.toml",
	"NODE_MODULES/pkg/index.js",
	"DIST/app.js",
	"Build/app.o",
	"sub/Node_Modules/nested/index.js",
	"sub/.ENV",
}

func TestBuildExcludesCaseVariantSecretPaths(t *testing.T) {
	root := t.TempDir()
	for _, rel := range caseVariantSecrets {
		write(t, root, rel, "canary\n", 0o600)
	}
	write(t, root, "app.go", "package app\n", 0o644)

	m := mustBuild(t, root)

	if got := paths(m); len(got) != 1 || got[0] != "app.go" {
		t.Fatalf("entries = %v, want only app.go", got)
	}
	canary := digest("canary\n")
	for _, e := range m.Entries {
		if e.SHA256 == canary {
			t.Fatalf("case-variant secret leaked through %q", e.Path)
		}
	}
	if m.Bytes != int64(len("package app\n")) {
		t.Fatalf("bytes = %d, want only the allowed file counted", m.Bytes)
	}

	t.Run("case-variant symlink target is excluded too", func(t *testing.T) {
		symlink(t, root, "leak", ".ENV")
		if _, err := Build(root, Policy{}); err == nil {
			t.Fatal("Build accepted a symlink into a case-variant excluded path")
		}
	})

	t.Run("allowed components keep their original spelling", func(t *testing.T) {
		// Only the comparison is case-normalized. Lowercasing a stored path would
		// silently rename the source and break the projection on a case-sensitive
		// server filesystem.
		spelled := t.TempDir()
		want := []string{"CamelCase.go", "Makefile", "README.MD", "Sub/File.TXT", "UPPER/deep/Mixed.Txt"}
		for _, rel := range want {
			write(t, spelled, rel, "content of "+rel+"\n", 0o644)
		}
		symlink(t, spelled, "Alias.GO", "CamelCase.go")

		m := mustBuild(t, spelled)
		got := paths(m)
		wantAll := append([]string{"Alias.GO"}, want...)
		slices.Sort(wantAll)
		if strings.Join(got, ",") != strings.Join(wantAll, ",") {
			t.Fatalf("entries = %v, want %v", got, wantAll)
		}
		if e := find(t, m, "Alias.GO"); e.Target != "CamelCase.go" {
			t.Fatalf("entry = %+v, want the original target spelling", e)
		}

		// The spelling must survive materialization too.
		dstPath := filepath.Join(t.TempDir(), "dst")
		if err := Materialize(spelled, m, openDest(t, dstPath)); err != nil {
			t.Fatalf("Materialize: %v", err)
		}
		for _, rel := range want {
			if _, err := os.Lstat(filepath.Join(dstPath, filepath.FromSlash(rel))); err != nil {
				t.Fatalf("materialized path %q missing: %v", rel, err)
			}
		}
		if verify := mustBuild(t, dstPath); verify.SHA256 != m.SHA256 {
			t.Fatalf("materialized digest = %s, want %s", verify.SHA256, m.SHA256)
		}
	})
}

func TestBuildRejectsCaseVariantNestedGitDirectory(t *testing.T) {
	for _, marker := range []string{".GIT", ".Git", ".gIt"} {
		t.Run(marker, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ".git/config", "[core]\n", 0o644)
			write(t, root, "app.go", "package app\n", 0o644)
			write(t, root, "vendor/dep/main.go", "package dep\n", 0o644)
			write(t, root, "vendor/dep/"+marker+"/config", "[core]\n", 0o644)

			m, err := Build(root, Policy{})
			if err == nil {
				t.Fatalf("Build accepted nested %q; entries %v", marker, paths(m))
			}
			if !strings.Contains(err.Error(), "vendor/dep") {
				t.Fatalf("error = %v, want it to name the nested repository", err)
			}
			if m.SHA256 != "" || len(m.Entries) != 0 {
				t.Fatalf("failed Build returned a partial manifest: %+v", m)
			}
		})
	}

	t.Run("case-variant root marker stays repository storage", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, ".GIT/config", "[core]\n", 0o644)
		write(t, root, ".GIT/objects/pack/pack-1.idx", "binary\n", 0o644)
		write(t, root, "app.go", "package app\n", 0o644)
		m := mustBuild(t, root)
		if got := paths(m); len(got) != 1 || got[0] != "app.go" {
			t.Fatalf("entries = %v, want only app.go", got)
		}
	})
}

func TestBuildRejectsCaseVariantNestedGitFile(t *testing.T) {
	for _, rel := range []string{"sub/.GIT", "a/b/c/.Git"} {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ".git/config", "[core]\n", 0o644)
			write(t, root, "app.go", "package app\n", 0o644)
			write(t, root, path.Dir(rel)+"/main.go", "package sub\n", 0o644)
			write(t, root, rel, "gitdir: ../.git/modules/sub\n", 0o644)

			m, err := Build(root, Policy{})
			if err == nil {
				t.Fatalf("Build accepted case-variant submodule marker %q; entries %v", rel, paths(m))
			}
			if !strings.Contains(err.Error(), path.Dir(rel)) {
				t.Fatalf("error = %v, want it to name %q", err, path.Dir(rel))
			}
		})
	}

	t.Run("case-variant root marker file stays repository storage", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "app.go", "package app\n", 0o644)
		write(t, root, ".GIT", "gitdir: /elsewhere/.git/worktrees/wt\n", 0o644)
		m := mustBuild(t, root)
		if got := paths(m); len(got) != 1 || got[0] != "app.go" {
			t.Fatalf("entries = %v, want only app.go", got)
		}
	})
}
