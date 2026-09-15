package machine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/record"
)

// imageArgs captures the whole source instance, disks included, as a machine
// image. It is the fork primitive: a snapshot would leave the data disk's
// contents to be reassembled, while a machine image creates a box that boots.
func imageArgs(cfg config.Config, image string, source box.Name) []string {
	return []string{
		"compute", "machine-images", "create", image,
		cfg.ProjectFlag(),
		"--source-instance=" + source.String(),
		"--source-instance-zone=" + cfg.Zone,
	}
}

// forkArgs creates the new box from the machine image. The image carries the
// machine type and the disks, so they are not repeated here: a fork must be a
// copy of the source, not of whatever the configuration says today. What is set
// explicitly is everything that has to differ from the source, which is the
// labels (they name the new box) and the lifetime bounds.
func forkArgs(cfg config.Config, target box.Name, image string) []string {
	args := append(instanceArgs(cfg, "create", target.String()), "--source-machine-image="+image)
	args = append(args, addressFlag(cfg)...)
	args = append(args, "--service-account="+cfg.ServiceAccount, "--scopes=cloud-platform")
	args = append(args, "--metadata="+metadata(cfg, false))
	args = append(args, boxFlags(cfg, target, true)...)
	return args
}

// runFork copies one box into another. The source's records must be settled
// first, because a machine image taken from a box whose last operation never
// concluded would copy an unknown state, and a fork is not the place to discover
// that the source was half-changed.
func runFork(ctx context.Context, deps cli.Deps, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: devbox machine fork <name> <new>")
	}
	source, err := box.ParseName(args[0])
	if err != nil {
		return err
	}
	target, err := box.ParseName(args[1])
	if err != nil {
		return err
	}
	if source == target {
		return fmt.Errorf("fork needs two names: %s cannot be forked onto itself", source)
	}
	if err := block(deps, source, target); err != nil {
		return err
	}
	if _, err := own(ctx, deps, source); err != nil {
		return err
	}
	if _, found, err := look(ctx, deps, target); err != nil {
		return err
	} else if found {
		return fmt.Errorf("box %s already exists in zone %s", target, deps.Config.Zone)
	}

	image := source.String() + "-" + stamp(time.Now())
	if _, err := mutate(ctx, deps, record.KindFork, source, imageArgs(deps.Config, image, source), "machine image "+image); err != nil {
		return err
	}
	create := forkArgs(deps.Config, target, image)
	if _, err := mutate(ctx, deps, record.KindFork, target, create, "instance "+target.String()); err != nil {
		deps.Errorf("machine image %s exists and is kept", image)
		deps.Errorf("  retry the new box by hand: gcloud %s", strings.Join(create, " "))
		return err
	}
	deps.Printf("forked %s to %s from machine image %s", source, target, image)
	adoptDataDisk(ctx, deps, target)
	return nil
}
