package machine

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/record"
)

// schedulePolicy is the instance schedule resource policy a box uses. It is
// derived from the box name so `new --schedule` can attach the same policy that
// `devbox schedule <name>` creates, in either order.
func schedulePolicy(name box.Name) string { return "devbox-" + name.String() + "-schedule" }

// addressFlag is the addressing of a box. devbox defaults to no external
// address, reached over an IAP tunnel, and the configuration's boolean cannot
// name a reserved address, so a box that asks for one gets the ephemeral address
// gcloud allocates when the flag is absent.
func addressFlag(cfg config.Config) []string {
	if cfg.ExternalIP {
		return nil
	}
	return []string{"--no-address"}
}

// metadata is the instance metadata every box carries: OS Login is always on,
// because that is how the access slice reaches the box, and the startup script is
// the retained bootstrap that runs on every boot.
func metadata(cfg config.Config, noBootstrap bool) string {
	parts := []string{"enable-oslogin=TRUE"}
	if !noBootstrap && cfg.BootstrapURL != "" {
		parts = append(parts, "startup-script-url="+cfg.BootstrapURL)
	}
	return strings.Join(parts, ",")
}

// boxFlags are the flags that make an instance a devbox box rather than a plain
// virtual machine: its labels, its network tags, and its lifetime bounds.
func boxFlags(cfg config.Config, name box.Name, maxRunDuration bool) []string {
	flags := []string{"--labels=" + box.LabelFlag(box.Labels(name))}
	if len(cfg.Tags) > 0 {
		flags = append(flags, "--tags="+strings.Join(cfg.Tags, ","))
	}
	if maxRunDuration && cfg.MaxRunDuration != "" {
		flags = append(flags, "--max-run-duration="+cfg.MaxRunDuration, "--instance-termination-action=STOP")
	}
	return flags
}

// createArgs is the exact invocation that builds a box: current Debian on an
// Intel general-purpose machine, a boot disk, and a persistent SSD data disk
// that is never deleted with the instance, because a machine image can only
// capture a forkable disk and the data disk is what a fork carries.
func createArgs(cfg config.Config, name box.Name, noBootstrap, schedule bool) []string {
	args := instanceArgs(cfg, "create", name.String())
	args = append(args,
		"--machine-type="+cfg.MachineType,
		"--image-project="+cfg.ImageProject,
		"--image-family="+cfg.ImageFamily,
		"--boot-disk-size="+strconv.Itoa(cfg.BootDiskGB)+"GB",
		"--boot-disk-type="+cfg.BootDiskType,
		fmt.Sprintf("--create-disk=name=%s,size=%dGB,type=%s,device-name=%s,auto-delete=no",
			cfg.DataDiskName, cfg.DataDiskGB, cfg.DataDiskType, cfg.DataDiskName),
	)
	args = append(args, addressFlag(cfg)...)
	args = append(args, "--service-account="+cfg.ServiceAccount, "--scopes=cloud-platform")
	args = append(args, "--metadata="+metadata(cfg, noBootstrap))
	args = append(args, boxFlags(cfg, name, true)...)
	if schedule {
		args = append(args, "--resource-policies="+schedulePolicy(name))
	}
	return args
}

// runNew creates one box. Every refusal happens before the first cloud call that
// could spend money: an unresolved record, a missing bootstrap script, and a name
// that is already taken.
func runNew(ctx context.Context, deps cli.Deps, args []string) error {
	set := deps.FlagSet("new")
	noBootstrap := set.Bool("no-bootstrap", false, "create a box with no startup script")
	schedule := set.Bool("schedule", false, "attach this box's schedule policy")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("usage: devbox machine new <name> [--no-bootstrap] [--schedule]")
	}
	name, err := box.ParseName(set.Arg(0))
	if err != nil {
		return err
	}
	if err := block(deps, name); err != nil {
		return err
	}
	if deps.Config.BootstrapURL == "" && !*noBootstrap {
		return fmt.Errorf("no bootstrap url is configured; set bootstrap_url in %s or pass --no-bootstrap for a bare box", deps.ConfigPath)
	}
	if _, found, err := look(ctx, deps, name); err != nil {
		return err
	} else if found {
		return fmt.Errorf("box %s already exists in zone %s", name, deps.Config.Zone)
	}
	argv := createArgs(deps.Config, name, *noBootstrap, *schedule)
	if _, err := mutate(ctx, deps, record.KindCreate, name, argv, "instance "+name.String()); err != nil {
		return err
	}
	deps.Printf("created box %s in %s", name, deps.Config.Zone)
	adoptDataDisk(ctx, deps, name)
	return nil
}
