// Package tree moves a working tree between the local machine and a box under a
// positive allowlist: what is transferred is exactly what the manifest declares,
// so secrets and excluded paths cannot leave the machine by accident.
//
// A handoff has two directions. Push copies the local tree to the box through a
// staging directory built by manifest.Materialize, and pull applies the tree that
// is on the box over the local tree under a rollback journal. Both directions
// record the digest of the projection they handed over, so a pull can tell a
// local tree that has not moved since the last handoff from one that has.
package tree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/manifest"
)

// Commands returns the tree verbs.
func Commands() []cli.Command {
	return []cli.Command{
		{
			Name:    "push",
			Summary: "Copy a local working tree to a box under the manifest allowlist",
			Usage:   "devbox push <name> [path] [--tree <tree>]",
			Help:    "Only the files the manifest declares are copied; excluded paths never leave this machine.",
			Run:     push,
		},
		{
			Name:    "pull",
			Summary: "Apply the tree on a box over the local tree under a rollback journal",
			Usage:   "devbox pull <name> [path] [--tree <tree>] [--force]",
			Help:    "Refuses when the local tree changed since the push. --force applies anyway; the rollback journal is kept until the apply verifies.",
			Run:     pull,
		},
		{
			Name:    "trees",
			Summary: "List the trees on a box with the digests recorded locally",
			Usage:   "devbox trees <name>",
			Run:     trees,
		},
	}
}

// openSession reaches one box. Every tree verb moves its bytes through the
// access slice, and substituting this one function in a test is what makes push,
// pull, and trees exercisable against a recording session instead of a live box.
//
// The dialer is given the cloud executor because it resolves the instance before
// it opens a session: a box that came back on another address still resolves, so
// no tree verb has to remember to refresh the SSH configuration first.
var openSession = func(ctx context.Context, deps cli.Deps, name box.Name) (access.Session, error) {
	return access.Dialer{
		Config: deps.Config,
		Cloud:  deps.Cloud,
		Out:    deps.Out,
		Err:    deps.Err,
		Stdin:  deps.Stdin,
	}.Open(ctx, name)
}

// resolveTarget reads the box name and the local tree root from the positional
// arguments of a push or pull command line, and returns the root as an absolute
// path: the state file keys a tree by that path, so two runs that name the same
// directory by different spellings still agree on which tree they mean.
func resolveTarget(usage string, positional []string) (box.Name, string, error) {
	if len(positional) == 0 || len(positional) > 2 {
		return "", "", fmt.Errorf("usage: %s", usage)
	}
	name, err := box.ParseName(positional[0])
	if err != nil {
		return "", "", err
	}
	root := "."
	if len(positional) == 2 {
		root = positional[1]
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", root, err)
	}
	return name, absolute, nil
}

// stateFilePath is where the handed-over digests live, under the state
// directory, beside the configuration and the mutation records.
func stateFilePath() (string, error) {
	return config.StatePath(stateFileName)
}

// openBox opens the session one verb reached its box with.
func openBox(ctx context.Context, deps cli.Deps, name box.Name) (access.Session, error) {
	session, err := openSession(ctx, deps, name)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", name, err)
	}
	return session, nil
}

// newStaging creates the directory one transfer stages into. A transfer stages
// exactly what a manifest declares and never the local tree at large, so the
// staging directory is the whole of what can leave or enter the machine.
func stagingDir(prefix string) (string, error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	return dir, nil
}

// fileCount is how many regular files a manifest declares. A handoff reports it
// beside the byte total, so the operator can tell an empty tree from a large one
// without walking it again.
func fileCount(m manifest.Manifest) int {
	count := 0
	for _, entry := range m.Entries {
		if entry.Kind == manifest.KindFile {
			count++
		}
	}
	return count
}

// entriesByPath indexes a manifest for the lookups an apply and a rollback do
// per path.
func entriesByPath(m manifest.Manifest) map[string]manifest.Entry {
	index := make(map[string]manifest.Entry, len(m.Entries))
	for _, entry := range m.Entries {
		index[entry.Path] = entry
	}
	return index
}
