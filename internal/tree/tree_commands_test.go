package tree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"devbox/internal/access"
)

// TestPushRejectsABadCommandLineBeforeItReachesTheBox covers the shapes a caller
// gets wrong: a missing box name, a box name that is not a box, and a tree name
// that is not one path component.
func TestPushRejectsABadCommandLineBeforeItReachesTheBox(t *testing.T) {
	cases := []struct {
		shape string
		argv  []string
		wants string
	}{
		{shape: "no box", argv: []string{"push"}, wants: "usage: devbox push <name> [path]"},
		{shape: "extra argument", argv: []string{"push", "dev", ".", "extra"}, wants: "usage: devbox push <name> [path]"},
		{shape: "box name", argv: []string{"push", "Dev Box"}, wants: "box name"},
		{shape: "tree name escapes the tree root", argv: []string{"push", "dev", ".", "--tree", "../../etc"}, wants: "tree name"},
		{shape: "tree name hides from the listing", argv: []string{"push", "dev", ".", "--tree", ".manifests"}, wants: "tree name"},
	}
	for _, test := range cases {
		t.Run(test.shape, func(t *testing.T) {
			recording := &access.Recording{}
			f := newFixture(t, recording)
			err := f.run(test.argv...)
			if err == nil {
				t.Fatalf("push accepted %v", test.argv)
			}
			if !strings.Contains(err.Error(), test.wants) {
				t.Fatalf("error %q does not mention %q", err, test.wants)
			}
			if len(recording.Commands)+len(recording.Uploads) != 0 {
				t.Fatalf("a refused command line reached the box: %v %v", recording.Commands, recording.Uploads)
			}
			if _, err := os.Stat(filepath.Join(f.state, stateFileName)); !os.IsNotExist(err) {
				t.Fatalf("a refused command line recorded a handoff: %v", err)
			}
		})
	}
}

// TestTreeCommandsUnderDryRunChangeNothing shows that the dry run reports the
// transfer it would make without staging, uploading, or recording anything.
func TestTreeCommandsUnderDryRunChangeNothing(t *testing.T) {
	root := treeFixture(t, "alpha", map[string]string{"a.txt": "one\n"})
	recording := &access.Recording{}
	f := newFixture(t, recording)
	f.dryRun = true

	if err := f.run("push", "dev", root); err != nil {
		t.Fatalf("push --dry-run: %v", err)
	}
	if len(recording.Commands)+len(recording.Uploads) != 0 {
		t.Fatalf("the dry run reached the box: %v %v", recording.Commands, recording.Uploads)
	}
	if _, err := os.Stat(filepath.Join(f.state, stateFileName)); !os.IsNotExist(err) {
		t.Fatalf("the dry run recorded a handoff: %v", err)
	}
	printed := f.out.String()
	for _, want := range []string{"dry run:", "~/devbox/trees/alpha", "1 files", "4 bytes"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the dry run printed %q, which does not report %q", printed, want)
		}
	}
}
