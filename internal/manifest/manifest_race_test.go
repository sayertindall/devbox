// Race harnesses for the containment tests. The subtests that drive them stay in
// manifest_build_test.go and manifest_materialize_test.go, so their names are
// unchanged; only the long harness bodies live here.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// classify reduces an error to the rule that produced it, so a race test can
// report which containment layer rejected each attempt.
func classify(err error) string {
	text := err.Error()
	for _, rule := range []string{
		"was replaced while it was read",
		"source was replaced after the manifest was built",
		"source content changed after the manifest was built",
		"source is no longer a regular file",
		"is no longer a directory",
		"was replaced while it was read",
		"absolute symlink target",
		"resolves outside root",
		"is dangling",
		"undeclared source path",
		"missing declared path",
		"content digest differs",
		"declared kind",
		"declared size",
		"special file",
		"no such file or directory",
	} {
		if strings.Contains(text, rule) {
			return rule
		}
	}
	return text
}

// runDirectoryReplacementRace repeatedly builds root while a churner swaps the
// declared "sub" directory for a symlink pointing outside the source root. No
// build may ever report a path outside the allowlist or read the outside secret.
func runDirectoryReplacementRace(t *testing.T, root, outside, secretDigest string) {
	t.Helper()
	write(t, root, "sub/keep.txt", "keep\n", 0o644)
	real := filepath.Join(root, "sub")
	// The stash must live outside the source root, so the root only ever
	// holds "sub" as a real directory or as a symlink pointing outside.
	stashed := filepath.Join(filepath.Dir(root), "sub-stash")

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			// Replace the directory with a symlink pointing outside root.
			if err := os.Rename(real, stashed); err == nil {
				_ = os.Symlink(outside, real)
				_ = os.Remove(real)
				_ = os.Rename(stashed, real)
			}
		}
	}()

	allowed := []string{"app.go", "sub/keep.txt"}
	var builds, failures int
	for range 400 {
		m, err := Build(root, Policy{})
		if err != nil {
			failures++
			continue
		}
		builds++
		for _, e := range m.Entries {
			if !slices.Contains(allowed, e.Path) {
				stop.Store(true)
				wg.Wait()
				t.Fatalf("Build produced unexpected path %q during a directory replacement race", e.Path)
			}
			if e.SHA256 == secretDigest {
				stop.Store(true)
				wg.Wait()
				t.Fatalf("Build read a file outside the source root at %q", e.Path)
			}
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("directory replacement race: %d successful builds, %d rejected builds", builds, failures)
	if builds == 0 {
		t.Fatal("no build succeeded; the race test proved nothing")
	}
}

// runConcurrentSourceReplacement churns the declared file app.go between two
// equal-length contents and a symlink to a secret outside the source root, while
// Build and Materialize run against it. No rejected materialization may leave
// unverified bytes, a temporary file, or outside content behind.
func runConcurrentSourceReplacement(t *testing.T, parent, root, outsideSecret string) {
	t.Helper()
	target := filepath.Join(root, "app.go")
	outsideSecretPath := filepath.Join(parent, "secret.txt")
	const a = "package aaa\n"
	const b = "package bbb\n"

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Both churners stage outside the source root and land with an atomic
	// rename, so neither adds an undeclared path of its own and neither
	// writes *through* the symlink into the outside secret.
	replace := func(name string, content []byte) {
		staged := filepath.Join(parent, name)
		if err := os.WriteFile(staged, content, 0o644); err != nil {
			return
		}
		_ = os.Rename(staged, target)
	}

	// Content churn: equal length, so only a digest can catch it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			content := a
			if i%2 == 0 {
				content = b
			}
			replace("content.stage", []byte(content))
		}
	}()

	// Type churn: the declared path flips between a regular file and a
	// symlink pointing at a secret outside the source root. That is the
	// window the Lstat/Open/SameFile protocol has to close.
	swap := filepath.Join(parent, "app.go.swap")
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if err := os.Symlink(outsideSecretPath, swap); err != nil {
				continue
			}
			if err := os.Rename(swap, target); err != nil {
				_ = os.Remove(swap)
				continue
			}
			// Remove the symlink itself before restoring a regular file, so
			// the outside secret is never written through.
			_ = os.Remove(target)
			replace("restore.stage", []byte(a))
		}
	}()

	// A rejected materialization must leave no unverified byte and no
	// temporary file behind, so the destination is inspected either way.
	inspect := func(dstPath string, m Manifest) error {
		declared := map[string]Entry{}
		for _, e := range m.Entries {
			declared[e.Path] = e
		}
		var problem error
		walkErr := filepath.WalkDir(dstPath, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(dstPath, p)
			if relErr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			entry, ok := declared[rel]
			if !ok {
				problem = fmt.Errorf("destination holds undeclared path %q", rel)
				return nil
			}
			if entry.Kind != KindFile {
				return nil
			}
			content, readErr := os.ReadFile(p)
			if readErr != nil {
				problem = fmt.Errorf("read %q: %w", rel, readErr)
				return nil
			}
			if digest(string(content)) != entry.SHA256 {
				problem = fmt.Errorf("destination path %q holds unverified bytes %q", rel, content)
			}
			return nil
		})
		if walkErr != nil {
			return walkErr
		}
		return problem
	}

	reasons := map[string]int{}
	var succeeded, rejected int
	for range 300 {
		m, err := Build(root, Policy{})
		if err != nil {
			rejected++
			reasons["build: "+classify(err)]++
			continue
		}
		dstPath := filepath.Join(t.TempDir(), "dst")
		materializeErr := Materialize(root, m, openDest(t, dstPath))
		if materializeErr != nil {
			rejected++
			reasons["materialize: "+classify(materializeErr)]++
		} else {
			succeeded++
		}
		if problem := inspect(dstPath, m); problem != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("after Materialize err=%v: %v", materializeErr, problem)
		}
		if treeContains(t, dstPath, digest(outsideSecret)) {
			stop.Store(true)
			wg.Wait()
			t.Fatal("outside secret content reached the destination during the race")
		}
		if materializeErr != nil {
			continue
		}
		// A successful materialization must contain every declared entry.
		for _, e := range m.Entries {
			if e.Kind != KindFile {
				continue
			}
			if _, statErr := os.Lstat(filepath.Join(dstPath, filepath.FromSlash(e.Path))); statErr != nil {
				stop.Store(true)
				wg.Wait()
				t.Fatalf("declared entry %q missing from destination: %v", e.Path, statErr)
			}
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("concurrent source replacement: %d verified materializations, %d rejected", succeeded, rejected)
	for reason, count := range reasons {
		t.Logf("  rejection %-42s %d", reason, count)
	}
	// The race phase is probabilistic, so the happy path is proven
	// separately once the churn has stopped.
	quiet, err := Build(root, Policy{})
	if err != nil {
		t.Fatalf("Build after the race: %v", err)
	}
	quietPath := filepath.Join(t.TempDir(), "dst")
	if err := Materialize(root, quiet, openDest(t, quietPath)); err != nil {
		t.Fatalf("Materialize after the race: %v", err)
	}
	if verify := mustBuild(t, quietPath); verify.SHA256 != quiet.SHA256 {
		t.Fatalf("quiet materialization digest = %s, want %s", verify.SHA256, quiet.SHA256)
	}
	// The projection must never have written through the symlink either.
	if content, err := os.ReadFile(outsideSecretPath); err != nil || string(content) != outsideSecret {
		t.Fatalf("outside secret was modified: %q, %v", content, err)
	}
}
