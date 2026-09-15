package tree

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"devbox/internal/box"
	"devbox/internal/cli"
)

// trees lists the trees that are on the box, each with the digest and the local
// path devbox recorded for it, so an operator can tell at a glance which trees
// the box holds and what the next pull of each one compares the local tree
// against.
func trees(ctx context.Context, deps cli.Deps, args []string) error {
	const usage = "devbox trees <name>"
	set := deps.FlagSet("trees")
	positional, err := cli.Parse(set, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: %s", usage)
	}
	name, err := box.ParseName(positional[0])
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
	if deps.DryRun {
		deps.Printf("dry run: would read %s on %s", remoteTreesRoot(), name)
		return nil
	}
	session, err := openBox(ctx, deps, name)
	if err != nil {
		return err
	}
	listed, err := session.Run(ctx, listRemoteTrees())
	if err != nil {
		return fmt.Errorf("list the trees on %s: %w", name, err)
	}
	names := treeNames(listed)
	if len(names) == 0 {
		deps.Printf("no trees on %s", name)
		return nil
	}
	for _, tree := range names {
		record, recorded := state.find(name.String(), tree)
		if !recorded {
			deps.Printf("%s  not recorded  -", tree)
			continue
		}
		deps.Printf("%s  %s  %s", tree, record.Digest, record.Path)
	}
	return nil
}

// treeNames reads one tree name per line of a tree root listing. A name that
// could not be a tree name on this machine is skipped rather than reported: the
// listing is a directory an operator can also write to by hand, and a stray file
// there is not a tree devbox can act on.
func treeNames(listed string) []string {
	seen := make(map[string]bool)
	var names []string
	for _, line := range strings.Split(listed, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || seen[name] {
			continue
		}
		if _, err := parseTreeName(name); err != nil {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
