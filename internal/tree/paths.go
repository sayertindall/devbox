package tree

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"devbox/internal/box"
)

// manifestsDir holds the manifest of every tree the box received, one file per
// tree, beside the trees themselves. The leading dot keeps it out of the tree
// listing, and no tree name may start with a dot, so no tree can collide with it.
const manifestsDir = ".manifests"

// stagedTree is the name of the staged payload inside a push staging directory.
const stagedTree = "tree"

// journalTimestamp is the UTC spelling of one journal directory. It is a file
// name component, so it carries no colon.
const journalTimestamp = "20060102T150405Z"

// stateFileName is the tree state file under the devbox state directory.
const stateFileName = "trees.json"

// remoteTreesRoot is the tree root on the box: box.TreeRoot under the operator's
// home, which is where the bootstrap slice creates it.
func remoteTreesRoot() string { return "~/" + box.TreeRoot }

// remoteTreeDir is one tree's directory on the box.
func remoteTreeDir(tree string) string { return remoteTreesRoot() + "/" + tree }

// remoteManifestDir is the box-side directory of the pushed manifests.
func remoteManifestDir() string { return remoteTreesRoot() + "/" + manifestsDir }

// listRemoteTrees reads the tree root on the box. Hidden entries hold the pushed
// manifests, so skipping them is what keeps this listing to trees, and an empty
// box has no tree root yet, which is not an error.
func listRemoteTrees() string {
	return "ls -1 " + remoteTreesRoot() + " 2>/dev/null || true"
}

// probeRemote reports whether one tree is on the box, so a pull fails with the
// missing tree rather than with whatever the download does about it.
func probeRemote(tree string) string {
	return "test -d " + remoteTreeDir(tree)
}

// prepareRemote replaces one tree's directory on the box.
//
// The old directory is removed rather than written over: a file left behind on
// the box would come back on the next pull as a file the local tree does not
// have. The removal is confined to that tree's own directory inside the devbox
// state root, and the tree name is validated before it is ever interpolated, so
// this command can name nothing else.
func prepareRemote(tree string) string {
	dir := remoteTreeDir(tree)
	return "rm -rf -- " + dir + " && mkdir -p -- " + dir + " " + remoteManifestDir()
}

// parseTreeName accepts one path component as a tree name. The name becomes a
// directory on the box, a file name in the state directory, and a word in a
// remote shell command, so it is restricted to characters that need no quoting,
// cannot name a parent directory, and cannot hide from the tree listing.
func parseTreeName(value string) (string, error) {
	if value == "" || len(value) > 64 {
		return "", errors.New("tree name must be 1 to 64 characters")
	}
	if strings.HasPrefix(value, ".") {
		return "", fmt.Errorf("tree name %q may not start with a dot", value)
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		letter := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
		digit := char >= '0' && char <= '9'
		if !letter && !digit && char != '-' && char != '_' && char != '.' {
			return "", fmt.Errorf("tree name %q may contain only letters, digits, dots, hyphens, and underscores", value)
		}
	}
	return value, nil
}

// resolveTreeName names a tree: the tree flag, or the base name of the local
// root, which is what an operator who pushed a checkout expects to find again.
func resolveTreeName(flagValue, root string) (string, error) {
	if flagValue != "" {
		return parseTreeName(flagValue)
	}
	name := filepath.Base(root)
	if name == "." || name == string(filepath.Separator) {
		return "", fmt.Errorf("cannot name a tree after %s: pass --tree", root)
	}
	return parseTreeName(name)
}
