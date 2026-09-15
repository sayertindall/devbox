package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
	"devbox/internal/record"
)

// machineImageFacts is the part of a machine image that proves ownership. A
// machine image has no labels of its own; it stores the source instance's
// properties, labels included, so those labels are what devbox checks before it
// deletes an image.
type machineImageFacts struct {
	Name               string `json:"name"`
	InstanceProperties struct {
		Labels map[string]string `json:"labels"`
	} `json:"instanceProperties"`
}

// diskFacts is the label set of one persistent disk.
type diskFacts struct {
	Labels map[string]string `json:"labels"`
}

func instanceDeleteArgs(cfg config.Config, name box.Name) []string {
	// --quiet: the deletion has already been confirmed against the inventory.
	return append(instanceArgs(cfg, "delete", name.String()), "--quiet")
}

func diskDeleteArgs(cfg config.Config, disk string) []string {
	return []string{"compute", "disks", "delete", disk, cfg.ProjectFlag(), cfg.ZoneFlag(), "--quiet"}
}

func imageDeleteArgs(cfg config.Config, image string) []string {
	return []string{"compute", "machine-images", "delete", image, cfg.ProjectFlag(), "--quiet"}
}

func imageListArgs(cfg config.Config) []string {
	return []string{"compute", "machine-images", "list", cfg.ProjectFlag(), "--format=json"}
}

func diskDescribeArgs(cfg config.Config, disk string) []string {
	return []string{"compute", "disks", "describe", disk, cfg.ProjectFlag(), cfg.ZoneFlag(), "--format=json"}
}

// imagesOf returns the machine images of one box. A name is not proof of
// ownership, so every image the list reports is re-checked against the labels it
// carries, and an image devbox cannot prove it made is left alone.
func imagesOf(ctx context.Context, deps cli.Deps, name box.Name) ([]string, error) {
	out, err := deps.Cloud.Run(ctx, imageListArgs(deps.Config)...)
	if err != nil {
		return nil, err
	}
	var images []machineImageFacts
	if err := json.Unmarshal([]byte(out), &images); err != nil {
		return nil, fmt.Errorf("decode machine images: %w", err)
	}
	var owned []string
	for _, image := range images {
		if owner, ok := box.NameFromLabels(image.InstanceProperties.Labels); ok && owner == name {
			owned = append(owned, image.Name)
		}
	}
	return owned, nil
}

// claimsDisk reports whether a data disk carries this box's labels, which is the
// only thing that lets destroy delete it.
func claimsDisk(ctx context.Context, deps cli.Deps, disk string, name box.Name) (bool, error) {
	out, err := deps.Cloud.Run(ctx, diskDescribeArgs(deps.Config, disk)...)
	if err != nil {
		if gcloud.Missing(err) {
			return false, nil
		}
		return false, err
	}
	// gcloud reports a single describe as a one-element list, like an instance.
	var facts []diskFacts
	if err := json.Unmarshal([]byte(out), &facts); err != nil {
		return false, fmt.Errorf("decode disk %s: %w", disk, err)
	}
	if len(facts) == 0 {
		return false, nil
	}
	owner, ok := box.NameFromLabels(facts[0].Labels)
	return ok && owner == name, nil
}

// runDestroy deletes a box and only what devbox labeled for it. The inventory is
// printed before the confirmation is checked, because an operator who has not
// confirmed yet still needs to see exactly what is at stake, and nothing is
// deleted until --confirm repeats the box name.
func runDestroy(ctx context.Context, deps cli.Deps, args []string) error {
	set := deps.FlagSet("destroy")
	confirm := set.String("confirm", "", "repeat the box name to confirm the deletion")
	withImages := set.Bool("with-images", false, "also delete the machine images labeled for this box")
	names, err := cli.Parse(set, args)
	if err != nil {
		return err
	}
	if len(names) != 1 {
		return fmt.Errorf("usage: devbox machine destroy <name> --confirm=<name> [--with-images]")
	}
	name, err := box.ParseName(names[0])
	if err != nil {
		return err
	}
	if deps.DryRun {
		deps.Printf("would read box %s and the resources labeled for it, then delete only those:", name)
		deps.Printf("would run: gcloud %s --format=json", strings.Join(instanceArgs(deps.Config, "describe", name.String()), " "))
		deps.Printf("would run: gcloud compute disks describe <its data disk> %s --format=json", deps.Config.ProjectFlag())
		deps.Printf("would run: gcloud compute instances delete %s --quiet, then the labeled disk, and any labeled image with --with-images", name)
		deps.Printf("snapshots are never deleted by destroy")
		if *confirm != name.String() {
			return fmt.Errorf("destroy needs --confirm=%s; nothing was deleted", name)
		}
		return nil
	}
	facts, err := own(ctx, deps, name)
	if err != nil {
		return err
	}
	disk := dataDiskName(facts)
	claimed := false
	if disk != "" {
		if claimed, err = claimsDisk(ctx, deps, disk, name); err != nil {
			return err
		}
	}
	images, err := imagesOf(ctx, deps, name)
	if err != nil {
		return err
	}

	deps.Printf("destroy %s would delete:", name)
	deps.Printf("  instance %s (%s) in %s", name, facts.Status, deps.Config.Zone)
	switch {
	case disk == "":
		deps.Printf("  no data disk is attached")
	case claimed:
		deps.Printf("  data disk %s", disk)
	default:
		deps.Printf("  data disk %s is not labeled for %s and will be kept", disk, name)
	}
	for _, image := range images {
		if *withImages {
			deps.Printf("  machine image %s", image)
			continue
		}
		deps.Printf("  machine image %s will be kept without --with-images", image)
	}
	deps.Printf("  snapshots of %s are never deleted by destroy", name)

	// Shown before it is checked: an operator who has not confirmed still needs to
	// see exactly what is at stake.
	if *confirm != name.String() {
		return fmt.Errorf("destroy needs --confirm=%s; nothing was deleted", name)
	}

	if _, err := mutate(ctx, deps, record.KindDelete, name, instanceDeleteArgs(deps.Config, name), ""); err != nil {
		return err
	}
	deps.Printf("%s", deps.Outcome("deleted instance "+name.String(), "would delete instance "+name.String()))
	if disk != "" && claimed {
		if _, err := mutate(ctx, deps, record.KindDelete, name, diskDeleteArgs(deps.Config, disk), ""); err != nil {
			return err
		}
		deps.Printf("%s", deps.Outcome("deleted data disk "+disk, "would delete data disk "+disk))
	}
	if *withImages {
		for _, image := range images {
			if _, err := mutate(ctx, deps, record.KindDelete, name, imageDeleteArgs(deps.Config, image), ""); err != nil {
				return err
			}
			deps.Printf("%s", deps.Outcome("deleted machine image "+image, "would delete machine image "+image))
		}
	}
	return nil
}
