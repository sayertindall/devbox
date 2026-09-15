package machine

import (
	"context"
	"fmt"
	"time"

	"devbox/internal/box"
	"devbox/internal/cli"
)

// runList reports the boxes devbox manages. Ownership is decided by the same
// label predicate that guards every other verb, so a virtual machine created by
// hand in the same project is never reported as a box.
func runList(ctx context.Context, deps cli.Deps, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: devbox machine list")
	}
	out, err := deps.Cloud.Run(ctx, "compute", "instances", "list", deps.Config.ProjectFlag(), "--format=json")
	if err != nil {
		return err
	}
	facts, err := box.Decode(out)
	if err != nil {
		return err
	}
	deps.Printf("NAME ZONE STATUS MACHINE ADDRESS")
	listed := 0
	for _, fact := range facts {
		name, ok := box.NameFromLabels(fact.Labels)
		if !ok {
			continue
		}
		listed++
		deps.Printf("%s %s %s %s %s", name, box.ZoneName(fact.Zone), fact.Status, box.MachineTypeName(fact.MachineType), address(fact))
	}
	if listed == 0 {
		deps.Printf("no boxes")
	}
	return nil
}

// runShow prints one box in full, including the data disk a snapshot would
// capture and whether an unfinished operation is holding the box back.
func runShow(ctx context.Context, deps cli.Deps, args []string) error {
	name, err := oneName("show", args)
	if err != nil {
		return err
	}
	facts, err := own(ctx, deps, name)
	if err != nil {
		return err
	}
	deps.Printf("name %s", name)
	deps.Printf("zone %s", box.ZoneName(facts.Zone))
	deps.Printf("status %s", facts.Status)
	deps.Printf("machine %s", box.MachineTypeName(facts.MachineType))
	if !facts.CreatedAt.IsZero() {
		deps.Printf("created %s", facts.CreatedAt.UTC().Format(time.RFC3339))
	}
	deps.Printf("labels %s", box.LabelFlag(facts.Labels))
	deps.Printf("internal %s", fallback(facts.InternalIP(), "none"))
	deps.Printf("external %s", fallback(facts.ExternalIP(), "none"))
	deps.Printf("data disk %s", fallback(dataDiskName(facts), "none"))
	entries, err := deps.Records.Unresolved(name.String())
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		deps.Printf("records none unresolved")
		return nil
	}
	deps.Printf("records %d unresolved", len(entries))
	for _, entry := range entries {
		deps.Printf("  %s %s %s", entry.ID, entry.Kind, entry.State)
	}
	return nil
}

// address prefers the external address, because that is the one an operator can
// reach from anywhere, and falls back to the internal one a tunnel uses.
func address(facts box.Facts) string {
	if external := facts.ExternalIP(); external != "" {
		return external
	}
	return fallback(facts.InternalIP(), "none")
}

func fallback(value, when string) string {
	if value == "" {
		return when
	}
	return value
}
