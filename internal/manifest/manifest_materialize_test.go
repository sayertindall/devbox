// Materialize-path tests: what lands in the destination, and what never does.
// The long concurrent source-replacement harness lives in manifest_race_test.go.
package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestMaterializeRejectsUndeclaredPath(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app.go", "package app\n", 0o644)
	write(t, root, "sub/keep.txt", "keep\n", 0o644)
	m := mustBuild(t, root)

	// The source grows a new allowed file after the manifest was built.
	write(t, root, "sub/sneaky.txt", "sneaky\n", 0o644)

	dstPath := filepath.Join(t.TempDir(), "dst")
	err := Materialize(root, m, openDest(t, dstPath))
	if err == nil {
		t.Fatal("Materialize accepted an undeclared source path")
	}
	if !strings.Contains(err.Error(), "sub/sneaky.txt") {
		t.Fatalf("error = %v, want it to name the undeclared path", err)
	}
	if _, statErr := os.Lstat(filepath.Join(dstPath, "sub", "sneaky.txt")); statErr == nil {
		t.Fatal("undeclared path was materialized")
	}
}

func TestMaterializeUsesPreopenedDestinationRoot(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app.go", "package app\n", 0o644)
	write(t, root, "sub/keep.txt", "keep\n", 0o644)
	write(t, root, "scripts/run.sh", "#!/bin/sh\n", 0o755)
	symlink(t, root, "alias.go", "app.go")
	m := mustBuild(t, root)

	t.Run("nil destination is rejected", func(t *testing.T) {
		if err := Materialize(root, m, nil); err == nil {
			t.Fatal("Materialize accepted a nil destination root")
		}
	})

	t.Run("destination path swapped after the root was opened", func(t *testing.T) {
		parent := t.TempDir()
		dstPath := filepath.Join(parent, "dst")
		destination := openDest(t, dstPath)

		// The caller already holds a trusted root. An attacker now redirects the
		// path that root was opened from: the real directory moves aside and the
		// old name becomes a symlink to a directory the caller never approved.
		moved := filepath.Join(parent, "moved")
		if err := os.Rename(dstPath, moved); err != nil {
			t.Fatalf("move destination: %v", err)
		}
		evil := filepath.Join(parent, "evil")
		if err := os.Mkdir(evil, 0o755); err != nil {
			t.Fatalf("mkdir evil: %v", err)
		}
		if err := os.Symlink(evil, dstPath); err != nil {
			t.Fatalf("symlink over destination path: %v", err)
		}

		if err := Materialize(root, m, destination); err != nil {
			t.Fatalf("Materialize: %v", err)
		}

		// Every byte must have landed through the pre-opened descriptor.
		content, err := os.ReadFile(filepath.Join(moved, "sub", "keep.txt"))
		if err != nil || string(content) != "keep\n" {
			t.Fatalf("file in the trusted destination = %q, %v", content, err)
		}
		entries, err := os.ReadDir(evil)
		if err != nil {
			t.Fatalf("read evil: %v", err)
		}
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("Materialize wrote through the swapped destination path: %v", names)
		}
		if verify := mustBuild(t, moved); verify.SHA256 != m.SHA256 {
			t.Fatalf("materialized tree digest = %s, want %s", verify.SHA256, m.SHA256)
		}
	})

	t.Run("no temporary file is left behind", func(t *testing.T) {
		dstPath := filepath.Join(t.TempDir(), "dst")
		if err := Materialize(root, m, openDest(t, dstPath)); err != nil {
			t.Fatalf("Materialize: %v", err)
		}
		err := filepath.WalkDir(dstPath, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if strings.Contains(d.Name(), "tmp") {
				t.Fatalf("temporary file left behind: %s", p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk destination: %v", err)
		}
	})
}

func TestMaterializeRejectsSourceReplacementAfterManifestBuild(t *testing.T) {
	const outsideSecret = "OUTSIDE SECRET\n"

	newSource := func(t *testing.T) (parent, root string, m Manifest) {
		t.Helper()
		parent = t.TempDir()
		if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte(outsideSecret), 0o600); err != nil {
			t.Fatalf("write outside secret: %v", err)
		}
		root = filepath.Join(parent, "project")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatalf("mkdir project: %v", err)
		}
		write(t, root, "app.go", "package app\n", 0o644)
		write(t, root, "sub/keep.txt", "keep\n", 0o644)
		return parent, root, mustBuild(t, root)
	}

	t.Run("file replaced by a symlink to an outside secret", func(t *testing.T) {
		_, root, m := newSource(t)
		if err := os.Remove(filepath.Join(root, "app.go")); err != nil {
			t.Fatalf("remove source file: %v", err)
		}
		symlink(t, root, "app.go", "../secret.txt")

		dstPath := filepath.Join(t.TempDir(), "dst")
		if err := Materialize(root, m, openDest(t, dstPath)); err == nil {
			t.Fatal("Materialize accepted a replaced source file")
		}
		if treeContains(t, dstPath, digest(outsideSecret)) {
			t.Fatal("outside secret content reached the destination")
		}
		if _, err := os.Lstat(filepath.Join(dstPath, "app.go")); err == nil {
			t.Fatal("replaced entry was materialized")
		}
	})

	t.Run("file content replaced with equal length", func(t *testing.T) {
		_, root, m := newSource(t)
		// Same length as the declared content, so only the digest can catch it.
		write(t, root, "app.go", "package bad\n", 0o644)

		dstPath := filepath.Join(t.TempDir(), "dst")
		err := Materialize(root, m, openDest(t, dstPath))
		if err == nil {
			t.Fatal("Materialize accepted replaced source content")
		}
		if treeContains(t, dstPath, digest("package bad\n")) {
			t.Fatal("tampered content reached the destination")
		}
	})

	t.Run("file replaced by a FIFO", func(t *testing.T) {
		_, root, m := newSource(t)
		if err := os.Remove(filepath.Join(root, "app.go")); err != nil {
			t.Fatalf("remove source file: %v", err)
		}
		if err := syscall.Mkfifo(filepath.Join(root, "app.go"), 0o600); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
		dstPath := filepath.Join(t.TempDir(), "dst")
		if err := Materialize(root, m, openDest(t, dstPath)); err == nil {
			t.Fatal("Materialize accepted a source file replaced by a FIFO")
		}
		if _, err := os.Lstat(filepath.Join(dstPath, "app.go")); err == nil {
			t.Fatal("FIFO-backed entry was materialized")
		}
	})

	t.Run("concurrent replacement never leaves unverified bytes", func(t *testing.T) {
		parent, root, _ := newSource(t)
		runConcurrentSourceReplacement(t, parent, root, outsideSecret)
	})
}

func TestMaterializeNeverWritesOutsideDestination(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app.go", "package app\n", 0o644)
	write(t, root, "sub/keep.txt", "keep\n", 0o644)
	write(t, root, "scripts/run.sh", "#!/bin/sh\n", 0o755)
	symlink(t, root, "alias.go", "app.go")
	good := mustBuild(t, root)

	t.Run("declared entries only", func(t *testing.T) {
		parent := t.TempDir()
		dstPath := filepath.Join(parent, "dst")
		if err := Materialize(root, good, openDest(t, dstPath)); err != nil {
			t.Fatalf("Materialize: %v", err)
		}
		content, err := os.ReadFile(filepath.Join(dstPath, "sub", "keep.txt"))
		if err != nil || string(content) != "keep\n" {
			t.Fatalf("copied file = %q, %v", content, err)
		}
		info, err := os.Lstat(filepath.Join(dstPath, "scripts", "run.sh"))
		if err != nil {
			t.Fatalf("lstat script: %v", err)
		}
		if info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("executable bit lost: %v", info.Mode())
		}
		target, err := os.Readlink(filepath.Join(dstPath, "alias.go"))
		if err != nil || target != "app.go" {
			t.Fatalf("symlink = %q, %v", target, err)
		}
		if verify := mustBuild(t, dstPath); verify.SHA256 != good.SHA256 {
			t.Fatalf("materialized tree digest = %s, want %s", verify.SHA256, good.SHA256)
		}
	})

	// Each rejection below must come from the manifest's own path rules, not
	// incidentally from the manifest-authority rewalk, so every case asserts
	// Validate rejects it as well as Materialize.
	for _, bad := range []struct {
		name  string
		entry Entry
		want  string
	}{
		{"escaping path entry", Entry{Path: "../escape.txt", Kind: KindFile, Size: 4, SHA256: digest("bad\n")}, "invalid relative path"},
		{"parent-traversal inside path", Entry{Path: "sub/../../escape.txt", Kind: KindFile, Size: 4, SHA256: digest("bad\n")}, "normalized relative path"},
		{"absolute path entry", Entry{Path: "/etc/passwd", Kind: KindFile, Size: 1, SHA256: digest("x")}, "normalized relative path"},
		{"escaping symlink target", Entry{Path: "sub/escape", Kind: KindSymlink, Target: "../../../etc/passwd"}, "outside root"},
		{"absolute symlink target", Entry{Path: "escape", Kind: KindSymlink, Target: "/etc/passwd"}, "absolute symlink target"},
		{"symlink target into excluded path", Entry{Path: "leak", Kind: KindSymlink, Target: ".ssh/id_ed25519"}, "excluded"},
		{"excluded entry path", Entry{Path: ".ssh/id_ed25519", Kind: KindFile, Size: 1, SHA256: digest("x")}, "excluded"},
		{"newline in path", Entry{Path: "a\nb.txt", Kind: KindFile, Size: 1, SHA256: digest("x")}, "invalid relative path"},
		{"unsupported kind", Entry{Path: "dev/null", Kind: "device", Size: 0}, "unsupported kind"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			parent := t.TempDir()
			dstPath := filepath.Join(parent, "dst")
			m := Manifest{Version: Version, Entries: []Entry{bad.entry}}

			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", bad.entry)
			}
			if !strings.Contains(err.Error(), bad.want) {
				t.Fatalf("Validate error = %v, want it to mention %q", err, bad.want)
			}
			if err := Materialize(root, m, openDest(t, dstPath)); err == nil {
				t.Fatalf("Materialize accepted %+v", bad.entry)
			}
			if _, err := os.Lstat(filepath.Join(parent, "escape.txt")); err == nil {
				t.Fatal("Materialize wrote outside the destination")
			}
			if _, err := os.Lstat(filepath.Join(dstPath, filepath.FromSlash(bad.entry.Path))); err == nil {
				t.Fatalf("rejected entry %q was still created", bad.entry.Path)
			}
		})
	}

	t.Run("digest mismatch", func(t *testing.T) {
		dstPath := filepath.Join(t.TempDir(), "dst")
		bad := good
		bad.Entries = append([]Entry(nil), good.Entries...)
		bad.SHA256 = ""
		for i := range bad.Entries {
			if bad.Entries[i].Kind == KindFile {
				bad.Entries[i].SHA256 = digest("not the real content")
				break
			}
		}
		if err := Materialize(root, bad, openDest(t, dstPath)); err == nil {
			t.Fatal("Materialize accepted a digest mismatch")
		}
		if treeContains(t, dstPath, digest("package app\n")) {
			t.Fatal("unverified bytes reached the destination")
		}
	})
}

func TestMaterializeRejectsCaseVariantExcludedPath(t *testing.T) {
	source := t.TempDir()
	write(t, source, "app.go", "package app\n", 0o644)

	for _, entry := range caseVariantManifestEntries() {
		t.Run(entry.Path+"->"+entry.Target, func(t *testing.T) {
			dstPath := filepath.Join(t.TempDir(), "dst")
			destination := openDest(t, dstPath)
			m := Manifest{Version: Version, Entries: []Entry{entry}, Bytes: entry.Size}

			// The rejection must come from the exclusion rule itself, not
			// incidentally from the manifest-authority rewalk of the source.
			err := Materialize(source, m, destination)
			if err == nil {
				t.Fatalf("Materialize accepted case-variant excluded entry %+v", entry)
			}
			if !strings.Contains(err.Error(), "excluded") {
				t.Fatalf("error = %v, want an exclusion error", err)
			}
			if _, err := os.Lstat(filepath.Join(dstPath, filepath.FromSlash(entry.Path))); err == nil {
				t.Fatalf("case-variant excluded entry %q was materialized", entry.Path)
			}
			left, err := os.ReadDir(dstPath)
			if err != nil {
				t.Fatalf("read destination: %v", err)
			}
			if len(left) != 0 {
				names := make([]string, 0, len(left))
				for _, d := range left {
					names = append(names, d.Name())
				}
				t.Fatalf("destination is not empty after rejection: %v", names)
			}
		})
	}
}
