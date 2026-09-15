package tree

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"devbox/internal/access"
	"devbox/internal/config"
	"devbox/internal/manifest"
)

// TestPullRefusesADivergedLocalTreeAndLeavesBothTreesAlone is the guarantee that
// keeps local work alive: a pull whose local tree is not the tree that was
// handed over changes nothing on either side, until the operator says force.
func TestPullRefusesADivergedLocalTreeAndLeavesBothTreesAlone(t *testing.T) {
	local := treeFixture(t, "alpha", map[string]string{"a.txt": "one\n", "b.txt": "two\n"})
	box := treeFixture(t, "alpha", map[string]string{"a.txt": "from the box\n", "c.txt": "new\n"})

	f := newFixture(t, newStaging(t))
	if err := f.run("push", "dev", local); err != nil {
		t.Fatalf("push: %v", err)
	}
	pushed := f.stateFile()

	// The operator works locally after the push, which is the case the refusal
	// exists for.
	writeFile(t, filepath.Join(local, "a.txt"), "local work\n")

	localBefore := snapshot(t, local)
	boxBefore := snapshot(t, box)
	session := &treeBox{Recording: &access.Recording{}, t: t, dir: box, tree: "alpha"}
	f.session = session

	err := f.run("pull", "dev", local)
	if err == nil {
		t.Fatal("pull applied a diverged local tree")
	}
	if !strings.Contains(err.Error(), "has changed since the last handoff") {
		t.Fatalf("error %q does not explain the divergence", err)
	}
	if len(session.Commands)+len(session.Downloads)+len(session.Uploads) != 0 {
		t.Fatalf("a refused pull reached the box: %v %v %v", session.Commands, session.Downloads, session.Uploads)
	}
	if got := snapshot(t, local); !maps.Equal(got, localBefore) {
		t.Fatalf("the refused pull changed the local tree: %v", got)
	}
	if got := snapshot(t, box); !maps.Equal(got, boxBefore) {
		t.Fatalf("the refused pull changed the tree on the box: %v", got)
	}
	if got := f.stateFile(); got.Trees[0].Digest != pushed.Trees[0].Digest {
		t.Fatalf("the refused pull moved the recorded digest to %s", got.Trees[0].Digest)
	}

	// The escape hatch applies the tree from the box over the local changes.
	if err := f.run("pull", "dev", local, "--force"); err != nil {
		t.Fatalf("pull --force: %v", err)
	}
	if got, want := snapshot(t, local), snapshot(t, box); !maps.Equal(got, want) {
		t.Fatalf("the forced pull left %v, want the tree from the box %v", got, want)
	}
	if !strings.Contains(f.errOut.String(), "force:") {
		t.Fatalf("the forced pull printed %q, which does not say what it overwrote", f.errOut.String())
	}
	wantCommands := []string{"test -d ~/devbox/trees/alpha"}
	if !slices.Equal(session.Commands, wantCommands) {
		t.Fatalf("the forced pull ran %v, want %v", session.Commands, wantCommands)
	}
	if len(session.Downloads) != 1 || session.Downloads[0][0] != "~/devbox/trees/alpha" {
		t.Fatalf("the forced pull downloaded %v, want the tree directory on the box", session.Downloads)
	}
}

// TestPullWithoutAHandoffAppliesOnlyUnderForce covers the other half of the
// refusal: a tree devbox has no record of at all, and a local root that does not
// exist yet, both of which a forced pull accepts.
func TestPullWithoutAHandoffAppliesOnlyUnderForce(t *testing.T) {
	local := treeFixture(t, "alpha", map[string]string{"a.txt": "one\n"})
	box := treeFixture(t, "alpha", map[string]string{"b.txt": "from the box\n"})

	f := newFixture(t, &treeBox{Recording: &access.Recording{}, t: t, dir: box, tree: "alpha"})
	err := f.run("pull", "dev", local)
	if err == nil {
		t.Fatal("pull applied a tree with no recorded handoff")
	}
	if !strings.Contains(err.Error(), "no handoff of tree alpha on dev is recorded") {
		t.Fatalf("error %q does not name the missing handoff", err)
	}
	if got := snapshot(t, local); !maps.Equal(got, map[string]string{"a.txt": "one\n"}) {
		t.Fatalf("the refused pull changed the local tree: %v", got)
	}

	if err := f.run("pull", "dev", local, "--force"); err != nil {
		t.Fatalf("pull --force: %v", err)
	}
	if got, want := snapshot(t, local), snapshot(t, box); !maps.Equal(got, want) {
		t.Fatalf("the forced pull left %v, want %v", got, want)
	}

	// A forced pull into a directory that does not exist is how the box tree
	// lands on a machine that has never had it.
	fresh := filepath.Join(t.TempDir(), "fresh")
	if err := f.run("pull", "dev", fresh, "--tree", "alpha", "--force"); err != nil {
		t.Fatalf("pull --force into a new directory: %v", err)
	}
	if got, want := snapshot(t, fresh), snapshot(t, box); !maps.Equal(got, want) {
		t.Fatalf("the forced pull left %v, want %v", got, want)
	}
}

// TestPullAppliesAdditionsReplacementsAndDeletions is a whole handoff in the
// return direction: a file the box changed, a file the box added, a file the box
// removed, and a directory the box turned into a file.
func TestPullAppliesAdditionsReplacementsAndDeletions(t *testing.T) {
	local := treeFixture(t, "alpha", map[string]string{"a.txt": "one\n", "dir/x.txt": "x\n"})
	box := treeFixture(t, "alpha", map[string]string{
		"a.txt":      "one changed\n",
		"c.txt":      "new\n",
		"dir":        "now a file\n",
		"d/link":     "-> ../a.txt",
		"d/other.js": "other\n",
	})

	f := newFixture(t, newStaging(t))
	if err := f.run("push", "dev", local); err != nil {
		t.Fatalf("push: %v", err)
	}
	f.session = &treeBox{Recording: &access.Recording{}, t: t, dir: box, tree: "alpha"}
	if err := f.run("pull", "dev", local); err != nil {
		t.Fatalf("pull: %v", err)
	}

	if got, want := snapshot(t, local), snapshot(t, box); !maps.Equal(got, want) {
		t.Fatalf("the pull left %v, want the tree from the box %v", got, want)
	}
	info, err := os.Lstat(filepath.Join(local, "dir"))
	if err != nil || info.IsDir() {
		t.Fatalf("the directory did not become the file that arrived: %v %v", info, err)
	}

	arrived, err := manifest.Build(box, manifest.Policy{})
	if err != nil {
		t.Fatalf("build the arrived manifest: %v", err)
	}
	if got := f.stateFile().Trees[0].Digest; got != arrived.SHA256 {
		t.Fatalf("the record holds digest %s, want the tree that arrived %s", got, arrived.SHA256)
	}
	if left := f.journal("dev", "alpha"); len(left) != 0 {
		t.Fatalf("a verified apply left a rollback journal behind: %v", left)
	}
	wantPrinted := fmt.Sprintf("pulled tree alpha from dev: 4 files, %d bytes\n", arrived.Bytes)
	if got := f.out.String(); got != wantPrinted {
		t.Fatalf("the pull printed %q, want %q", got, wantPrinted)
	}
}

// TestPullRestoresTheLocalTreeWhenTheApplyFails drives the rollback journal
// through a failure that lands after the apply has already changed the tree: a
// file the box replaced, a file the box added, and a file the box turned into a
// directory are in place when the write of a path that is still a local directory
// fails. Every one of them has to be undone, and the bytes of the deleted file
// have to come back from the journal.
func TestPullRestoresTheLocalTreeWhenTheApplyFails(t *testing.T) {
	local := treeFixture(t, "alpha", map[string]string{
		"a.txt":     "one\n",
		"b.txt":     "two\n",
		"d":         "a file the box turned into a directory\n",
		"keep/.env": "SECRET=local\n",
	})
	box := treeFixture(t, "alpha", map[string]string{
		"a.txt":       "one changed\n",
		"c.txt":       "new\n",
		"d/inner.txt": "inner\n",
		"keep":        "the box has a file where the directory was\n",
	})

	f := newFixture(t, newStaging(t))
	if err := f.run("push", "dev", local); err != nil {
		t.Fatalf("push: %v", err)
	}
	pushed := f.stateFile()
	localBefore := snapshot(t, local)
	boxBefore := snapshot(t, box)

	f.session = &treeBox{Recording: &access.Recording{}, t: t, dir: box, tree: "alpha"}
	err := f.run("pull", "dev", local)
	if err == nil {
		t.Fatal("pull applied a tree over a local directory that still holds a file")
	}
	if !strings.Contains(err.Error(), "materialize keep") {
		t.Fatalf("error %q does not name the path whose write failed, so the apply did not get past the earlier entries", err)
	}
	if !strings.Contains(err.Error(), "the local tree was restored from the rollback journal") {
		t.Fatalf("error %q does not say the local tree was restored", err)
	}
	if got := snapshot(t, local); !maps.Equal(got, localBefore) {
		t.Fatalf("the failed apply left the local tree as %v, want %v", got, localBefore)
	}
	if info, err := os.Lstat(filepath.Join(local, "d")); err != nil || info.IsDir() {
		t.Fatalf("the file the box turned into a directory did not come back: %v %v", info, err)
	}
	if got := snapshot(t, box); !maps.Equal(got, boxBefore) {
		t.Fatalf("the failed apply changed the tree on the box: %v", got)
	}
	if got := f.stateFile().Trees[0].Digest; got != pushed.Trees[0].Digest {
		t.Fatalf("the failed apply moved the recorded digest to %s", got)
	}
	if left := f.journal("dev", "alpha"); len(left) != 0 {
		t.Fatalf("a restored tree left its journal behind: %v", left)
	}
}

// TestPullLeavesExcludedLocalPathsAlone is the return direction of the allowlist:
// a pull deletes and rewrites only what a manifest declares, so a secret and a
// dependency tree beside the files it does rewrite survive untouched.
func TestPullLeavesExcludedLocalPathsAlone(t *testing.T) {
	local := treeFixture(t, "alpha", map[string]string{
		"a.txt":                     "one\n",
		"b.txt":                     "two\n",
		".env":                      "SECRET=local\n",
		"node_modules/dep/index.js": "module.exports = 1;\n",
	})
	box := treeFixture(t, "alpha", map[string]string{"a.txt": "changed\n"})

	f := newFixture(t, newStaging(t))
	if err := f.run("push", "dev", local); err != nil {
		t.Fatalf("push: %v", err)
	}
	f.session = &treeBox{Recording: &access.Recording{}, t: t, dir: box, tree: "alpha"}
	if err := f.run("pull", "dev", local); err != nil {
		t.Fatalf("pull: %v", err)
	}

	got := snapshot(t, local)
	want := map[string]string{
		"a.txt":                     "changed\n",
		".env":                      "SECRET=local\n",
		"node_modules/dep/index.js": "module.exports = 1;\n",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("the pull left %v, want %v", got, want)
	}
}

// TestPullRefusesAnUnreadableStateFile keeps a state file devbox cannot trust
// from being written over: the pull stops before it touches either tree.
func TestPullRefusesAnUnreadableStateFile(t *testing.T) {
	local := treeFixture(t, "alpha", map[string]string{"a.txt": "one\n"})
	box := treeFixture(t, "alpha", map[string]string{"a.txt": "changed\n"})

	f := newFixture(t, &treeBox{Recording: &access.Recording{}, t: t, dir: box, tree: "alpha"})
	path, err := config.StatePath(stateFileName)
	if err != nil {
		t.Fatalf("state path: %v", err)
	}
	if err := os.WriteFile(path, []byte("this is not json\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	localBefore := snapshot(t, local)

	err = f.run("pull", "dev", local)
	if err == nil {
		t.Fatal("pull read a state file it cannot decode")
	}
	if !strings.Contains(err.Error(), "decode tree state") {
		t.Fatalf("error %q does not name the state file", err)
	}
	if got := snapshot(t, local); !maps.Equal(got, localBefore) {
		t.Fatalf("the refused pull changed the local tree: %v", got)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "this is not json\n" {
		t.Fatalf("the refused pull rewrote the state file: %q %v", data, err)
	}
}
