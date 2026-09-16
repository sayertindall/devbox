package tree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"devbox/internal/cli"
	"devbox/internal/manifest"
)

// push copies the local tree to the box: it builds the manifest of the local
// root, writes exactly the declared entries into a staging directory, sends that
// directory and the manifest that describes it, and records the digest the next
// pull compares the local tree against.
func push(ctx context.Context, deps cli.Deps, args []string) error {
	const usage = "devbox push <name> [path] [--tree <tree>]"
	set := deps.FlagSet("push")
	treeFlag := set.String("tree", "", "tree name on the box (default: the base name of the local path)")
	everything := set.Bool("everything", false,
		"send the directory exactly as it is on disk, dependency and build directories included; the default leaves those behind")
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
	policy := manifest.Policy{Mode: manifest.ModeWorkingCopy}
	if *everything {
		policy.Mode = manifest.ModeVerbatim
	}
	pushed, err := manifest.Build(root, policy)
	if err != nil {
		return fmt.Errorf("build the manifest of %s: %w", root, err)
	}
	if deps.DryRun {
		deps.Printf("dry run: would replace %s on %s", remoteTreeDir(treeName), name)
		deps.Printf("dry run: would upload %d files, %d bytes to %s:%s", fileCount(pushed), pushed.Bytes, name, remoteTreeDir(treeName))
		deps.Printf("dry run: would record digest %s for tree %s", pushed.SHA256, treeName)
		return nil
	}

	session, err := openBox(ctx, deps, name)
	if err != nil {
		return err
	}
	work, err := stageTree(root, treeName, pushed, policy)
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	deps.Printf("replacing %s on %s", remoteTreeDir(treeName), name)
	if _, err := session.Run(ctx, prepareRemote(treeName)); err != nil {
		return fmt.Errorf("replace %s on %s: %w", remoteTreeDir(treeName), name, err)
	}
	if err := session.Upload(ctx, filepath.Join(work, stagedTree), remoteTreeDir(treeName)); err != nil {
		return fmt.Errorf("upload the tree to %s:%s: %w", name, remoteTreeDir(treeName), err)
	}
	if err := session.Upload(ctx, filepath.Join(work, treeName+".json"), remoteManifestDir()); err != nil {
		return fmt.Errorf("upload the manifest to %s:%s: %w", name, remoteManifestDir(), err)
	}

	state, err := loadState(statePath)
	if err != nil {
		return err
	}
	state.record(newState(name.String(), treeName, pushed.SHA256, root, policy.Mode, time.Now()))
	if err := saveState(statePath, state); err != nil {
		return err
	}
	if *everything {
		deps.Printf("the directory was sent exactly as it is on disk, dependency and build directories included")
	}
	deps.Printf("pushed tree %s to %s: %d files, %d bytes", treeName, name, fileCount(pushed), pushed.Bytes)
	return nil
}

// stageTree writes exactly the declared entries of root into a fresh staging
// directory and returns it. The directory holds the payload under stagedTree and
// the manifest beside it, so one upload carries the tree and one carries the
// declaration the box keeps for it.
func stageTree(root, treeName string, pushed manifest.Manifest, policy manifest.Policy) (_ string, err error) {
	work, err := stagingDir("devbox-push-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(work)
		}
	}()

	payload := filepath.Join(work, stagedTree)
	if err := os.Mkdir(payload, 0o755); err != nil {
		return "", fmt.Errorf("create staging tree: %w", err)
	}
	destination, err := os.OpenRoot(payload)
	if err != nil {
		return "", fmt.Errorf("open staging tree: %w", err)
	}
	defer destination.Close()
	if err := manifest.Materialize(root, pushed, policy, destination); err != nil {
		return "", fmt.Errorf("stage the tree: %w", err)
	}
	if err := manifest.Write(filepath.Join(work, treeName+".json"), pushed); err != nil {
		return "", fmt.Errorf("write the pushed manifest: %w", err)
	}
	return work, nil
}
