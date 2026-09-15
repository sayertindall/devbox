package tree

import (
	"fmt"
	"slices"
	"testing"

	"devbox/internal/access"
)

// TestTreesListsTheTreesOnTheBoxWithTheirRecordedDigests shows what the box
// holds beside what devbox recorded locally: the tree that was pushed carries its
// digest and local path, a tree that came from somewhere else is listed as
// unrecorded, and the hidden manifest directory is not a tree.
func TestTreesListsTheTreesOnTheBoxWithTheirRecordedDigests(t *testing.T) {
	root := treeFixture(t, "alpha", map[string]string{"a.txt": "one\n"})
	f := newFixture(t, newStaging(t))
	if err := f.run("push", "dev", root); err != nil {
		t.Fatalf("push: %v", err)
	}

	listing := "ls -1 ~/devbox/trees 2>/dev/null || true"
	recording := &access.Recording{Reply: func(command string) (string, error) {
		return "beta\nalpha\n.manifests\n", nil
	}}
	f.session = recording

	if err := f.run("trees", "dev"); err != nil {
		t.Fatalf("trees: %v", err)
	}
	digest := f.stateFile().Trees[0].Digest
	want := fmt.Sprintf("alpha  %s  %s\nbeta  not recorded  -\n", digest, root)
	if got := f.out.String(); got != want {
		t.Fatalf("trees printed %q, want %q", got, want)
	}
	wantCommands := []string{listing}
	if !slices.Equal(recording.Commands, wantCommands) {
		t.Fatalf("trees ran %v, want %v", recording.Commands, wantCommands)
	}
}

// TestTreesOnAnEmptyBox covers the box that has never received a tree: the tree
// root is not there yet, which is an answer rather than a failure.
func TestTreesOnAnEmptyBox(t *testing.T) {
	recording := &access.Recording{}
	f := newFixture(t, recording)
	if err := f.run("trees", "dev"); err != nil {
		t.Fatalf("trees: %v", err)
	}
	if got, want := f.out.String(), "no trees on dev\n"; got != want {
		t.Fatalf("trees printed %q, want %q", got, want)
	}
}
