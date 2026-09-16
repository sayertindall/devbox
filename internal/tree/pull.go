package tree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/manifest"
)

// pull applies the tree that is on the box over the local tree.
//
// It refuses while the local tree is not the tree devbox last handed over for
// this box and name, so a pull cannot quietly overwrite local work that has not
// been sent anywhere. When it does apply, it applies through a journal that
// holds the states worth keeping of every path the apply can destroy, verifies
// the result against the manifest that arrived, and puts the local tree back
// from the journal if anything about the apply fails.
func pull(ctx context.Context, deps cli.Deps, args []string) error {
	const usage = "devbox pull <name> [path] [--tree <tree>] [--force]"
	set := deps.FlagSet("pull")
	treeFlag := set.String("tree", "", "tree name on the box (default: the base name of the local path)")
	force := set.Bool("force", false, "apply the tree from the box even when the local tree has changed since the last handoff")
	positional, err := cli.Parse(set, args)
	if err != nil {
		return err
	}
	name, root, err := resolveTarget(usage, positional)
	if err != nil {
		return err
	}
	treeName, err := resolveTreeName(*treeFlag, root)
	if err != nil {
		return err
	}
	statePath, err := stateFilePath()
	if err != nil {
		return err
	}
	state, err := loadState(statePath)
	if err != nil {
		return err
	}
	record, recorded := state.find(name.String(), treeName)
	includeNested := recorded && record.IncludeNested
	before, err := localManifest(root, includeNested, *force)
	if err != nil {
		return err
	}
	if err := checkDivergence(deps, name, treeName, root, before, record, recorded, *force); err != nil {
		return err
	}
	if deps.DryRun {
		deps.Printf("dry run: would download %s:%s", name, remoteTreeDir(treeName))
		deps.Printf("dry run: would apply it over %s under a rollback journal", root)
		return nil
	}

	session, err := openBox(ctx, deps, name)
	if err != nil {
		return err
	}
	if _, err := session.Run(ctx, probeRemote(treeName)); err != nil {
		return fmt.Errorf("tree %s is not on %s (push it first): %w", treeName, name, err)
	}
	work, err := stagingDir("devbox-pull-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if err := session.Download(ctx, remoteTreeDir(treeName), work); err != nil {
		return fmt.Errorf("download %s from %s: %w", remoteTreeDir(treeName), name, err)
	}
	arrivedRoot := arrivedTree(work, treeName)
	if info, err := os.Stat(arrivedRoot); err != nil || !info.IsDir() {
		return fmt.Errorf("the download of %s from %s brought no tree", remoteTreeDir(treeName), name)
	}
	arrived, err := manifest.Build(arrivedRoot, manifest.Policy{})
	if err != nil {
		return fmt.Errorf("build the manifest of the tree received from %s: %w", name, err)
	}
	// The tree on the box is not signed by anything, so it is validated on
	// arrival exactly as a manifest built locally would be: an entry that escapes
	// the root, claims a namespace twice, or names an excluded path is refused
	// here, before any of it reaches the local tree.
	if err := arrived.Validate(); err != nil {
		return fmt.Errorf("validate the tree received from %s: %w", name, err)
	}

	journalDir, err := config.StatePath("journal", name.String(), treeName, time.Now().UTC().Format(journalTimestamp))
	if err != nil {
		return err
	}
	rollback, err := newJournal(journalDir, root, before, arrived, includeNested)
	if err != nil {
		return err
	}
	if err := applyArrived(root, arrivedRoot, before, arrived); err != nil {
		if restoreErr := rollback.restore(root, arrived); restoreErr != nil {
			return fmt.Errorf("%w; the local tree could not be restored: %v; the saved files are in %s", err, restoreErr, journalDir)
		}
		return fmt.Errorf("%w; the local tree was restored from the rollback journal", err)
	}
	if err := rollback.discard(); err != nil {
		deps.Errorf("warning: the rollback journal %s was not removed: %v", journalDir, err)
	}

	// The local tree is the tree that arrived, so the digest of record is now
	// that tree's: the next pull compares the local tree against it.
	state.record(newState(name.String(), treeName, arrived.SHA256, root, includeNested, time.Now()))
	if err := saveState(statePath, state); err != nil {
		return err
	}
	deps.Printf("pulled tree %s from %s: %d files, %d bytes", treeName, name, fileCount(arrived), arrived.Bytes)
	return nil
}

// checkDivergence refuses a pull whose local tree is not the tree devbox last
// handed over for this box and name. The force flag turns each refusal into a
// printed note: the operator has said to apply the box tree over whatever is
// here, and the note says what is being written over.
func checkDivergence(deps cli.Deps, name box.Name, tree, root string, before manifest.Manifest, record State, recorded, force bool) error {
	switch {
	case !recorded:
		if force {
			deps.Errorf("force: no handoff of tree %s on %s is recorded", tree, name)
			return nil
		}
		return fmt.Errorf("no handoff of tree %s on %s is recorded: push it first, or pass --force to apply the tree from the box over %s", tree, name, root)
	case record.Path != root:
		if force {
			deps.Errorf("force: tree %s on %s was recorded for %s, not %s", tree, name, record.Path, root)
			return nil
		}
		return fmt.Errorf("tree %s on %s was recorded for the local tree %s, not %s: push from %s first, or pass --force", tree, name, record.Path, root, record.Path)
	case record.Digest != before.SHA256:
		if force {
			deps.Errorf("force: %s has changed since the last handoff of tree %s on %s (local %s, recorded %s)", root, tree, name, before.SHA256, record.Digest)
			return nil
		}
		return fmt.Errorf("the local tree %s has changed since the last handoff of tree %s on %s (local %s, recorded %s): push the local changes first, or pass --force", root, tree, name, before.SHA256, record.Digest)
	}
	return nil
}

// arrivedTree is where a downloaded tree landed inside the staging directory.
// The session copies the remote directory into the staging directory, so scp and
// rsync both place it there under its own name; a session that copies the
// contents lands them directly in the staging directory instead.
func arrivedTree(staging, tree string) string {
	nested := filepath.Join(staging, tree)
	if info, err := os.Stat(nested); err == nil && info.IsDir() {
		return nested
	}
	return staging
}

// applyArrived replaces the local tree with the tree that arrived from the box,
// and touches exactly three sets of paths:
//
//   - the paths the local manifest declares and the arrived manifest does not,
//     which are removed;
//   - the paths the arrived manifest declares, which Materialize writes;
//   - a local directory standing where the arrived tree declares a file, and only
//     when that directory is empty.
//
// Every path in those sets comes from a manifest walk, and a walk never admits an
// excluded path, so no excluded path can be named here and none can be deleted
// or rewritten. Nothing is ever removed recursively: a directory that still
// holds anything at all is left for the write below to refuse, which is what
// keeps an excluded file inside it out of reach.
func applyArrived(root, arrivedRoot string, before, arrived manifest.Manifest) error {
	arrivedByPath := entriesByPath(arrived)

	// A manifest never declares a path below another declared path: a walk does
	// not descend into a symlink, and validation refuses a path claimed twice.
	// The removals therefore have no order to respect.
	for _, entry := range before.Entries {
		if _, retained := arrivedByPath[entry.Path]; retained {
			continue
		}
		if err := os.Remove(filepath.Join(root, entry.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", entry.Path, err)
		}
	}

	for _, entry := range arrived.Entries {
		path := filepath.Join(root, entry.Path)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		// An empty directory here is one the removals above emptied, so removing
		// it is what lets a directory become a file. A directory that still holds
		// local content is left in place, and the write below refuses that path.
		if err := os.Remove(path); err != nil && !errors.Is(err, syscall.ENOTEMPTY) {
			return fmt.Errorf("clear the directory %s before writing the file it became: %w", entry.Path, err)
		}
	}

	destination, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open local tree %s: %w", root, err)
	}
	defer destination.Close()
	if err := manifest.Materialize(arrivedRoot, arrived, manifest.Policy{}, destination); err != nil {
		return fmt.Errorf("write the tree that arrived: %w", err)
	}

	after, err := manifest.Build(root, manifest.Policy{})
	if err != nil {
		return fmt.Errorf("read the applied tree back: %w", err)
	}
	if err := manifest.Compare(arrived, after); err != nil {
		return fmt.Errorf("the applied tree does not match the manifest that arrived: %w", err)
	}
	return nil
}
