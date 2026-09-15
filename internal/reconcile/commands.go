// Package reconcile clears the records that block a box.
//
// A record becomes unresolved when a cloud call left the machine unable to tell
// what happened: the connection dropped, the CLI failed in a way devbox cannot
// interpret, or the process died between the request and the reply. Every other
// slice refuses to touch that box until the operator has looked at the cloud and
// said what the outcome was. This package is the only writer of that verdict, so
// the recovery path exists in one place instead of once per slice.
package reconcile

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"devbox/internal/cli"
	"devbox/internal/record"
)

// Commands returns the reconciliation verbs.
func Commands() []cli.Command {
	return []cli.Command{{
		Name:       "reconcile",
		Summary:    "List or clear the records that block a box",
		Usage:      "devbox reconcile [<record-id>] [--box <name>] [--note <text>]",
		ConfigOnly: true,
		Help: `With no arguments it lists every unresolved record with the command it
recorded, so you can check the cloud. Clearing one needs --note, so an unblock
always says who checked what.`,
		Run: run,
	}}
}

func run(_ context.Context, deps cli.Deps, args []string) error {
	set := deps.FlagSet("reconcile")
	boxName := set.String("box", "", "limit the listing to one box")
	note := set.String("note", "", "what you checked, recorded with the verdict")
	rest, err := cli.Parse(set, args)
	if err != nil {
		return err
	}
	switch len(rest) {
	case 0:
		return list(deps, *boxName)
	case 1:
		return clear(deps, rest[0], *note)
	default:
		return fmt.Errorf("usage: devbox reconcile [<record-id>] [--box <name>] [--note <text>]")
	}
}

// list prints every unresolved record, with the recorded command so the operator
// can see exactly what was attempted and what to check.
func list(deps cli.Deps, boxName string) error {
	entries, err := deps.Records.All()
	if err != nil {
		return err
	}
	var unresolved []record.Record
	for _, entry := range entries {
		if entry.State != record.StatePending && entry.State != record.StateUnknown {
			continue
		}
		if boxName != "" && entry.Box != boxName {
			continue
		}
		unresolved = append(unresolved, entry)
	}
	if len(unresolved) == 0 {
		deps.Printf("no unresolved records")
		return nil
	}
	sort.Slice(unresolved, func(i, j int) bool { return unresolved[i].CreatedAt.Before(unresolved[j].CreatedAt) })
	for _, entry := range unresolved {
		deps.Printf("%s  %s %s  %s", entry.ID, entry.Kind, entry.Box, entry.State)
		if entry.Detail != "" {
			deps.Printf("  detail: %s", entry.Detail)
		}
		if len(entry.Args) > 0 {
			deps.Printf("  recorded: gcloud %s", strings.Join(entry.Args, " "))
		}
	}
	deps.Printf("")
	deps.Printf("check the cloud state, then clear one with:")
	deps.Printf("  devbox reconcile <record-id> --note \"what you saw\"")
	return nil
}

// clear records the operator's verdict for one record.
func clear(deps cli.Deps, id, note string) error {
	entry, err := deps.Records.Load(id)
	if err != nil {
		return err
	}
	if entry.State == record.StateKnown {
		deps.Printf("%s is already resolved", id)
		return nil
	}
	if strings.TrimSpace(note) == "" {
		return fmt.Errorf("reconcile %s needs --note, recording what you checked before the box is unblocked", id)
	}
	if err := deps.Records.Resolved(entry, note); err != nil {
		return err
	}
	deps.Printf("cleared %s (%s %s)", entry.ID, entry.Kind, entry.Box)
	return nil
}
