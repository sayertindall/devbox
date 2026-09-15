package tree

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"devbox/internal/manifest"
)

// TestPushUploadsExactlyTheManifestDeclaredSet is the allowlist guarantee of a
// push: what was staged, and therefore what could leave the machine, is the
// manifest's own set, while a secret and a dependency tree in the same directory
// are nowhere in it.
func TestPushUploadsExactlyTheManifestDeclaredSet(t *testing.T) {
	root := treeFixture(t, "alpha", map[string]string{
		"main.go":                       "package main\n",
		"sub/util.go":                   "package sub\n",
		"sub/.env":                      "SECRET=nested\n",
		".env":                          "SECRET=local\n",
		"node_modules/pkg/index.js":     "module.exports = 1;\n",
		"sub/node_modules/dep/index.js": "module.exports = 2;\n",
		"dist/bundle.js":                "void 0;\n",
		".git/config":                   "[core]\n",
		"assets/logo.png":               "png\n",
		"link":                          "-> main.go",
	})
	session := newStaging(t)
	f := newFixture(t, session)
	if err := f.run("push", "dev", root, "--tree", "alpha"); err != nil {
		t.Fatalf("push: %v", err)
	}

	pushed, err := manifest.Build(root, manifest.Policy{})
	if err != nil {
		t.Fatalf("build the manifest the push should have sent: %v", err)
	}
	want := []string{"assets", "assets/logo.png", "link", "main.go", "sub", "sub/util.go"}
	if !slices.Equal(session.entries, want) {
		t.Fatalf("the staged tree is %v, want exactly the declared set %v", session.entries, want)
	}

	if len(session.Uploads) != 2 {
		t.Fatalf("the push made %d uploads, want the tree and its manifest: %v", len(session.Uploads), session.Uploads)
	}
	if got, wantDestination := session.Uploads[0][1], "~/devbox/trees/alpha"; got != wantDestination {
		t.Fatalf("the tree went to %q, want %q", got, wantDestination)
	}
	if got, wantName := filepath.Base(session.Uploads[0][0]), "tree"; got != wantName {
		t.Fatalf("the uploaded directory is %q, want %q", got, wantName)
	}
	if got, wantDestination := session.Uploads[1][1], "~/devbox/trees/.manifests"; got != wantDestination {
		t.Fatalf("the manifest went to %q, want %q", got, wantDestination)
	}
	if got, wantName := filepath.Base(session.Uploads[1][0]), "alpha.json"; got != wantName {
		t.Fatalf("the uploaded manifest is %q, want %q", got, wantName)
	}

	// The manifest the box keeps is the canonical serialization of the same
	// projection, which is the form its digest is computed over.
	encoded, err := manifest.Encode(pushed)
	if err != nil {
		t.Fatalf("encode the pushed manifest: %v", err)
	}
	if got := string(session.files["alpha.json"]); got != string(encoded)+"\n" {
		t.Fatalf("the box was given manifest %q, want %q", got, string(encoded)+"\n")
	}

	// The box-side copy is replaced rather than written over, so a file deleted
	// locally cannot come back on the next pull.
	wantCommands := []string{"rm -rf -- ~/devbox/trees/alpha && mkdir -p -- ~/devbox/trees/alpha ~/devbox/trees/.manifests"}
	if !slices.Equal(session.Commands, wantCommands) {
		t.Fatalf("the push ran %v on the box, want %v", session.Commands, wantCommands)
	}
	wantPrinted := fmt.Sprintf("replacing ~/devbox/trees/alpha on dev\npushed tree alpha to dev: 3 files, %d bytes\n", pushed.Bytes)
	if got := f.out.String(); got != wantPrinted {
		t.Fatalf("the push printed %q, want %q", got, wantPrinted)
	}
}

// TestPushRecordsTheHandoffTheNextPullCompares checks the state file itself: the
// digest, the local path, and the box and tree it belongs to, and the fact that a
// second handoff of the same tree replaces the record instead of adding one.
func TestPushRecordsTheHandoffTheNextPullCompares(t *testing.T) {
	root := treeFixture(t, "alpha", map[string]string{"a.txt": "one\n"})
	f := newFixture(t, newStaging(t))
	if err := f.run("push", "dev", root); err != nil {
		t.Fatalf("push: %v", err)
	}

	first, err := manifest.Build(root, manifest.Policy{})
	if err != nil {
		t.Fatalf("build the manifest: %v", err)
	}
	file := f.stateFile()
	if file.Version != stateVersion || len(file.Trees) != 1 {
		t.Fatalf("the state file holds %+v, want one record", file)
	}
	record := file.Trees[0]
	if record.Box != "dev" || record.Tree != "alpha" {
		t.Fatalf("the record names %s/%s, want dev/alpha", record.Box, record.Tree)
	}
	if record.Digest != first.SHA256 {
		t.Fatalf("the record holds digest %s, want the pushed %s", record.Digest, first.SHA256)
	}
	if record.Path != root {
		t.Fatalf("the record holds path %s, want %s", record.Path, root)
	}
	if _, err := time.Parse(time.RFC3339, record.UpdatedAt); err != nil {
		t.Fatalf("the record timestamp %q is not a time: %v", record.UpdatedAt, err)
	}

	writeFile(t, root+"/a.txt", "two\n")
	if err := f.run("push", "dev", root, "--tree", "alpha"); err != nil {
		t.Fatalf("second push: %v", err)
	}
	second, err := manifest.Build(root, manifest.Policy{})
	if err != nil {
		t.Fatalf("build the second manifest: %v", err)
	}
	if second.SHA256 == first.SHA256 {
		t.Fatal("the fixture did not change between the two pushes")
	}
	file = f.stateFile()
	if len(file.Trees) != 1 {
		t.Fatalf("two handoffs of one tree left %d records: %+v", len(file.Trees), file.Trees)
	}
	if file.Trees[0].Digest != second.SHA256 {
		t.Fatalf("the record holds digest %s, want the latest %s", file.Trees[0].Digest, second.SHA256)
	}
}
