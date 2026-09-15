package machine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"devbox/internal/access"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/record"
)

// quiesceStop and quiesceStart bracket a snapshot. Docker and containerd are
// stopped because a disk snapshot is crash-consistent, not application-consistent:
// a container image layer or a containerd content blob written while the disk is
// captured can be torn, and the damage only appears when the disk is used again.
const (
	quiesceStop  = "sudo systemctl stop docker containerd"
	quiesceFlush = "sudo sync"
	quiesceStart = "sudo systemctl start docker containerd"
)

// snapshotArgs captures the data disk, which carries the Docker root, the
// containerd root, and the caches: the boot disk is reproducible from the image
// and the bootstrap, so it is not worth a snapshot.
func snapshotArgs(cfg config.Config, name, disk string) []string {
	return []string{
		"compute", "snapshots", "create", name,
		cfg.ProjectFlag(),
		"--source-disk=" + disk,
		"--source-disk-zone=" + cfg.Zone,
	}
}

// runSnapshot stops the storage services on the box, captures its data disk, and
// starts them again whatever the snapshot did: leaving a box with docker stopped
// because a snapshot failed turns a recoverable failure into an outage.
func runSnapshot(ctx context.Context, deps cli.Deps, args []string) error {
	name, err := oneName("snapshot", args)
	if err != nil {
		return err
	}
	if deps.DryRun {
		// Everything here is derivable without reading the box, and the source disk
		// is the one devbox created, so the rehearsal needs no cloud call at all.
		deps.Printf("would run on %s: %s && %s", name, quiesceStop, quiesceFlush)
		deps.Printf("would run: gcloud %s", strings.Join(snapshotArgs(deps.Config, name.String(), deps.Config.DataDiskName), " "))
		deps.Printf("would run on %s: %s", name, quiesceStart)
		return nil
	}
	facts, err := own(ctx, deps, name)
	if err != nil {
		return err
	}
	if !facts.Running() {
		return fmt.Errorf("box %s is %s; start it before snapshotting so docker and containerd can be stopped first", name, facts.Status)
	}
	disk := dataDiskName(facts)
	if disk == "" {
		return fmt.Errorf("box %s has no attached data disk to snapshot", name)
	}

	// A dry run must not touch the box, so the services are only quiesced when a
	// snapshot will really be taken.
	var session access.Session
	if !deps.DryRun {
		session, err = openSession(ctx, deps, name)
		if err != nil {
			return err
		}
		if _, err := session.Run(ctx, quiesceStop); err != nil {
			return fmt.Errorf("stop docker and containerd on %s: %w", name, err)
		}
		// The services are stopped, but the page cache is not on the disk yet, and
		// the snapshot is taken from the disk.
		if _, err := session.Run(ctx, quiesceFlush); err != nil {
			return fmt.Errorf("flush writes on %s: %w", name, err)
		}
	}

	snapshot := name.String() + "-" + stamp(time.Now())
	_, snapshotErr := mutate(ctx, deps, record.KindSnapshot, name, snapshotArgs(deps.Config, snapshot, disk), "snapshot "+snapshot)
	// The services come back whatever the capture did: leaving a box without its
	// storage services turns a recoverable failure into an outage.
	var restartErr error
	if session != nil {
		if _, err := session.Run(ctx, quiesceStart); err != nil {
			restartErr = fmt.Errorf("restart docker and containerd on %s: %w", name, err)
		}
	}
	if snapshotErr != nil {
		// The snapshot failure is the reason to report, but a box left without its
		// services is what the operator has to fix first.
		if restartErr != nil {
			deps.Errorf("%v", restartErr)
		}
		return snapshotErr
	}
	if restartErr != nil {
		return restartErr
	}
	deps.Printf("%s", deps.Outcome("snapshot "+snapshot, "would create snapshot "+snapshot))
	return nil
}
