// This file holds only the fixtures the manifest test files share. The tests
// themselves live in manifest_build_test.go, manifest_validate_test.go,
// manifest_materialize_test.go, and manifest_race_test.go.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if err := os.Chmod(full, mode); err != nil {
		t.Fatalf("chmod %s: %v", rel, err)
	}
}

func symlink(t *testing.T, root, rel, target string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatalf("symlink %s: %v", rel, err)
	}
}

func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func paths(m Manifest) []string {
	out := make([]string, 0, len(m.Entries))
	for _, e := range m.Entries {
		out = append(out, e.Path)
	}
	return out
}

func find(t *testing.T, m Manifest, p string) Entry {
	t.Helper()
	for _, e := range m.Entries {
		if e.Path == p {
			return e
		}
	}
	t.Fatalf("manifest is missing %q; has %v", p, paths(m))
	return Entry{}
}

func mustBuild(t *testing.T, root string) Manifest {
	t.Helper()
	m, err := Build(root, Policy{})
	if err != nil {
		t.Fatalf("Build(%s): %v", root, err)
	}
	return m
}

// openDest creates a destination directory and opens the trusted root the
// caller must hand to Materialize.
func openDest(t *testing.T, dir string) *os.Root {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir destination: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open destination root: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// caseSensitive reports whether dir lives on a case-sensitive filesystem.
// APFS on macOS is case-insensitive by default, which makes a real on-disk
// case collision impossible to create there.
func caseSensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err != nil
}

// treeContains reports whether any regular file below dir has the given digest.
func treeContains(t *testing.T, dir, wantDigest string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		content, readErr := os.ReadFile(p)
		if readErr == nil && digest(string(content)) == wantDigest {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return found
}
