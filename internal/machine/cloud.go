// Package machine owns the box lifecycle: create, start, stop, suspend, resume,
// list, inspect, snapshot, fork, schedule, and destroy.
package machine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
	"devbox/internal/record"
)

// openSession opens a shell session on a box. Only snapshot needs one, to stop
// the storage services before the disk is captured; the access slice owns the
// real ssh transport. It is a variable so a test can run snapshot against a
// recording session and no test needs a live box.
var openSession = func(deps cli.Deps, name box.Name) (access.Session, error) {
	return access.Dialer{Config: deps.Config, Out: deps.Out, Err: deps.Err, Stdin: deps.Stdin}.Open(name)
}

// instanceArgs is the prefix every zonal instance call shares, so the argument
// vector reads in the same order as the gcloud documentation.
func instanceArgs(cfg config.Config, verb, name string) []string {
	return []string{"compute", "instances", verb, name, cfg.ProjectFlag(), cfg.ZoneFlag()}
}

// region is the region a box's disks and schedule live in, derived from the zone
// so a --zone override cannot leave a regional object in the wrong region.
func region(cfg config.Config) string {
	if derived := config.RegionFromZone(cfg.Zone); derived != cfg.Zone {
		return derived
	}
	return cfg.Region
}

// describe reads one instance. The error is returned unwrapped so callers can ask
// gcloud.Missing whether the box simply is not there.
func describe(ctx context.Context, deps cli.Deps, name box.Name) (box.Facts, error) {
	out, err := deps.Cloud.Run(ctx, append(instanceArgs(deps.Config, "describe", name.String()), "--format=json")...)
	if err != nil {
		return box.Facts{}, err
	}
	return box.Fact(out)
}

// look reports whether a box exists. An absent instance is a fact, not an error:
// it is how new and fork decide that a name is free.
func look(ctx context.Context, deps cli.Deps, name box.Name) (box.Facts, bool, error) {
	facts, err := describe(ctx, deps, name)
	if err == nil {
		return facts, true, nil
	}
	if gcloud.Missing(err) {
		return box.Facts{}, false, nil
	}
	return box.Facts{}, false, err
}

// own reads a box devbox created. Every verb that acts on a box goes through
// this, so an instance whose labels do not name the box devbox was asked about
// is refused instead of started, snapshotted, or deleted by mistake.
func own(ctx context.Context, deps cli.Deps, name box.Name) (box.Facts, error) {
	facts, found, err := look(ctx, deps, name)
	if err != nil {
		return box.Facts{}, err
	}
	if !found {
		return box.Facts{}, fmt.Errorf("box %s does not exist in zone %s", name, deps.Config.Zone)
	}
	owner, ok := box.NameFromLabels(facts.Labels)
	if !ok {
		return box.Facts{}, fmt.Errorf("instance %s carries no devbox labels; devbox did not create it and will not act on it", name)
	}
	if owner != name {
		return box.Facts{}, fmt.Errorf("instance %s is labeled for box %s; devbox will not act on it as %s", name, owner, name)
	}
	return facts, nil
}

// block refuses a mutation while a previous one for the same box never
// concluded, and names the records so the operator can reconcile them by hand.
func block(deps cli.Deps, names ...box.Name) error {
	var lines []string
	for _, name := range names {
		entries, err := deps.Records.Unresolved(name.String())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			lines = append(lines, fmt.Sprintf("  %s %s %s: gcloud %s", entry.ID, entry.Kind, entry.State, strings.Join(entry.Args, " ")))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("unresolved records block this box; check the cloud state and reconcile them first:\n%s", strings.Join(lines, "\n"))
}

// mutate runs one recorded cloud mutation and returns the command output.
//
// A dry run only prints the argument vector: recording a mutation that never
// left the machine would block the next real command. A real call writes its
// argument vector to a durable record before it starts, so an interrupted run
// leaves a named resource behind rather than a mystery.
func mutate(ctx context.Context, deps cli.Deps, kind string, name box.Name, argv []string, result string) (string, error) {
	if deps.DryRun {
		return deps.Cloud.Run(ctx, argv...)
	}
	entry, err := deps.Records.Begin(kind, name.String(), argv)
	if err != nil {
		return "", err
	}
	out, err := deps.Cloud.Run(ctx, argv...)
	if err != nil {
		return "", conclude(deps, entry, result, err)
	}
	if err := deps.Records.Known(entry, result); err != nil {
		return "", err
	}
	return out, nil
}

// conclude records what a mutation did. A missing resource proves the call
// changed nothing, which frees the next command to run. Any other failure leaves
// the outcome ambiguous, so the record stays unresolved, the next mutation for
// that box is refused, and the exact command to check by hand is printed.
func conclude(deps cli.Deps, entry record.Record, result string, err error) error {
	if gcloud.Missing(err) {
		if writeErr := deps.Records.Failed(entry, err.Error()); writeErr != nil {
			return writeErr
		}
		return err
	}
	if writeErr := deps.Records.Unknown(entry, err.Error()); writeErr != nil {
		return writeErr
	}
	deps.Errorf("record %s is unresolved; check the resource before the next mutation:", entry.ID)
	deps.Errorf("  gcloud %s", strings.Join(entry.Args, " "))
	return err
}

// exists reports whether a describe found its resource. A missing resource is a
// fact rather than an error, which is how a verb stays idempotent.
func exists(ctx context.Context, deps cli.Deps, argv []string) (bool, error) {
	if _, err := deps.Cloud.Run(ctx, argv...); err != nil {
		if gcloud.Missing(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// dataDiskName reads the data disk's name from the instance description. The
// attached disk carries it in the source URL, and that name, not the device name
// devbox asked for, is what a snapshot needs: a box created from a machine image
// has a disk name the image chose.
func dataDiskName(facts box.Facts) string {
	for _, disk := range facts.Disks {
		if disk.Boot || disk.Source == "" {
			continue
		}
		parts := strings.Split(disk.Source, "/")
		return parts[len(parts)-1]
	}
	return ""
}

// adoptDataDisk labels the data disk of a freshly created box, so destroy can
// prove the disk belongs to that box before it deletes it. The create flag that
// makes the disk has no labels field, so this is a second call; when it fails the
// box is still usable and the operator gets the command to label the disk.
func adoptDataDisk(ctx context.Context, deps cli.Deps, name box.Name) {
	if deps.DryRun {
		return
	}
	facts, err := describe(ctx, deps, name)
	if err != nil {
		deps.Errorf("box %s was created but its data disk could not be read back: %v", name, err)
		return
	}
	disk := dataDiskName(facts)
	if disk == "" {
		deps.Errorf("box %s has no data disk; destroy will keep nothing for it", name)
		return
	}
	argv := diskLabelArgs(deps.Config, disk, name)
	if _, err := mutate(ctx, deps, record.KindCreate, name, argv, "data disk "+disk+" labeled for "+name.String()); err != nil {
		deps.Errorf("data disk %s is not labeled for box %s, so destroy will keep it: %v", disk, name, err)
		deps.Errorf("  label it by hand: gcloud %s", strings.Join(argv, " "))
	}
}

// diskLabelArgs marks a data disk as belonging to one box. Ownership is by
// label, never by name or by attachment, so this call is what gives destroy the
// right to delete the disk later.
func diskLabelArgs(cfg config.Config, disk string, name box.Name) []string {
	return []string{"compute", "disks", "update", disk, cfg.ProjectFlag(), cfg.ZoneFlag(), "--update-labels=" + box.LabelFlag(box.Labels(name))}
}

// stamp is the UTC suffix every derived resource name carries, in a form that is
// legal in a Compute name and sorts with the moment it was taken.
func stamp(now time.Time) string { return now.UTC().Format("20060102-150405") }

// oneName parses the single box name a verb takes.
func oneName(verb string, args []string) (box.Name, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("usage: devbox machine %s <name>", verb)
	}
	return box.ParseName(args[0])
}
