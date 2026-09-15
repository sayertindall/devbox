package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/record"
)

// startupFileName is the artifact the operator's state directory holds, so a
// script can be read, diffed, and uploaded by hand without running devbox.
const startupFileName = "startup-script.sh"

// quiesceStop, quiesceFlush, and quiesceStart bracket a disk capture. Docker and
// containerd are stopped because a disk image is crash-consistent, not
// application-consistent: an image layer or a content blob written while the
// disk is read can be torn, and the damage only shows when the disk is used
// again.
const (
	quiesceStop  = "sudo systemctl stop docker containerd"
	quiesceFlush = "sudo sync"
	quiesceStart = "sudo systemctl start docker containerd"
)

// openSession opens a shell session on a box, which bake needs to stop the
// storage services before it captures the data disk. The access slice owns the
// real transport; it describes the instance through the same recorded cloud the
// command uses, so a session reaches a box that came back on a different
// address. This is a variable so a test can run a bake against a recording
// session and no test needs a live box.
var openSession = func(ctx context.Context, deps cli.Deps, name box.Name) (access.Session, error) {
	return access.Dialer{
		Config: deps.Config,
		Cloud:  deps.Cloud,
		Out:    deps.Out,
		Err:    deps.Err,
		Stdin:  deps.Stdin,
	}.Open(ctx, name)
}

// Commands returns the bootstrap verbs.
func Commands() []cli.Command {
	return []cli.Command{
		{
			Name:    "bootstrap",
			Summary: "Render and publish the startup script that builds a box",
			Usage:   "devbox bootstrap show | devbox bootstrap upload [--bucket gs://bucket]",
			// show prints the script; upload checks the settings it needs itself.
			ConfigOnly: true,
			Help: `show    prints the startup script a new box would run
upload  writes it to the state directory, and with --bucket publishes it and
        records the resulting url so machine new uses it`,
			Run: runBootstrap,
		},
		{
			Name:    "image",
			Summary: "Bake a reusable custom image from the data disk of a box",
			Usage:   "devbox image bake <name> <image>",
			Run:     runImage,
		},
		{
			Name:       "toolchain",
			Summary:    "Print the toolchain a box installs",
			Usage:      "devbox toolchain",
			ConfigOnly: true,
			Run:        runToolchain,
		},
	}
}

func runBootstrap(ctx context.Context, deps cli.Deps, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devbox bootstrap [show|upload]")
	}
	switch args[0] {
	case "show":
		return bootstrapShow(deps, args[1:])
	case "upload":
		return bootstrapUpload(ctx, deps, args[1:])
	default:
		return fmt.Errorf("usage: devbox bootstrap [show|upload]")
	}
}

// bootstrapShow prints the script the current configuration renders, which is
// what the box will run on its next boot.
func bootstrapShow(deps cli.Deps, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: devbox bootstrap show")
	}
	script, err := Render(deps.Config)
	if err != nil {
		return err
	}
	fmt.Fprint(deps.Out, script)
	return nil
}

// bootstrapUpload writes the rendered script into the state directory and, when
// it has a bucket to publish to, copies it there and records the object as the
// configuration's bootstrap url.
//
// A local path is deliberately not written to bootstrap_url: the machine slice
// passes that value to the instance as startup-script-url, which only accepts a
// gs:// or https location. With no bucket the command prints the exact copy
// command instead of leaving a url that would fail on the next `devbox machine
// new`.
func bootstrapUpload(ctx context.Context, deps cli.Deps, args []string) error {
	set := deps.FlagSet("bootstrap upload")
	bucket := set.String("bucket", "", "Cloud Storage bucket to publish the script in, for example gs://my-bucket")
	rest, err := cli.Parse(set, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("usage: devbox bootstrap upload [--bucket gs://bucket]")
	}
	if *bucket != "" && !strings.HasPrefix(*bucket, "gs://") {
		return fmt.Errorf("bucket %q must be a gs:// location, which is what an instance can fetch a startup script from", *bucket)
	}
	script, err := Render(deps.Config)
	if err != nil {
		return err
	}
	// The publish call needs a project, and only a project: this verb is how
	// bootstrap_url itself gets set, so requiring the whole configuration here
	// would make the command refuse the one job it exists to do.
	if *bucket != "" && strings.TrimSpace(deps.Config.Project) == "" {
		return fmt.Errorf("publishing needs a project: set project in %s (devbox tools edit opens it)", deps.ConfigPath)
	}
	path, err := config.StatePath(startupFileName)
	if err != nil {
		return err
	}
	if deps.DryRun {
		deps.Printf("would write %s (%d bytes)", path, len(script))
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create the state directory: %w", err)
		}
		if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
			return fmt.Errorf("write the startup script: %w", err)
		}
		deps.Printf("wrote %s", path)
	}

	target := *bucket
	if target == "" {
		target = bootstrapBucket(deps.Config.BootstrapURL)
	}
	if target == "" {
		deps.Printf("no bootstrap url is configured yet; publish the script with:")
		deps.Printf("  gcloud storage cp %s gs://<bucket>/%s", path, startupFileName)
		deps.Printf("or let devbox publish it and record the url: devbox bootstrap upload --bucket gs://<bucket>")
		return nil
	}
	object := strings.TrimSuffix(target, "/") + "/" + startupFileName
	argv := []string{"storage", "cp", path, object}
	if deps.DryRun {
		deps.Printf("dry run: gcloud %s", strings.Join(argv, " "))
		deps.Printf("dry run: %s is unchanged", deps.ConfigPath)
		return nil
	}
	if _, err := deps.Cloud.Run(ctx, argv...); err != nil {
		return fmt.Errorf("publish the startup script: %w", err)
	}
	cfg := deps.Config
	cfg.BootstrapURL = object
	if err := config.Save(deps.ConfigPath, cfg); err != nil {
		return err
	}
	deps.Printf("bootstrap_url %s", cfg.BootstrapURL)
	return nil
}

// bootstrapBucket reduces a configured bootstrap url to its bucket, so a second
// upload publishes to the place the first one chose. A local path or an empty
// value yields nothing: publishing needs a bucket.
func bootstrapBucket(url string) string {
	const scheme = "gs://"
	if !strings.HasPrefix(url, scheme) {
		return ""
	}
	trimmed := strings.TrimSuffix(strings.TrimPrefix(url, scheme), "/")
	if index := strings.Index(trimmed, "/"); index >= 0 {
		trimmed = trimmed[:index]
	}
	if trimmed == "" {
		return ""
	}
	return scheme + trimmed
}

func runImage(ctx context.Context, deps cli.Deps, args []string) error {
	if len(args) == 0 || args[0] != "bake" {
		return fmt.Errorf("usage: devbox image bake <name> <image>")
	}
	return imageBake(ctx, deps, args[1:])
}

// imageBake creates a custom image from a box's data disk: the disk carries the
// Docker root, the containerd root, and the caches, so an image of it plus the
// checked-in startup script is a machine that starts ready to work.
//
// The capture is bracketed by a quiesce when the box is running, because a disk
// image taken under a writing engine can carry a torn layer. A stopped box is
// already consistent, so it is captured as it is.
func imageBake(ctx context.Context, deps cli.Deps, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: devbox image bake <name> <image>")
	}
	name, err := box.ParseName(args[0])
	if err != nil {
		return err
	}
	image := args[1]
	if err := checkImageName(image); err != nil {
		return err
	}
	facts, err := own(ctx, deps, name)
	if err != nil {
		return err
	}
	disk := facts.DataDisk()
	if disk == "" {
		return fmt.Errorf("box %s has no attached data disk to capture", name)
	}
	unresolved, err := deps.Records.Unresolved(name.String())
	if err != nil {
		return err
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("box %s has %d unresolved records; clear them with devbox reconcile --box %s before baking an image", name, len(unresolved), name)
	}
	deps.Printf("baking image %s from data disk %s of box %s in %s", image, disk, name, deps.Config.Zone)
	if facts.Running() {
		deps.Printf("box %s is running: stopping docker and containerd for the capture", name)
	}

	var session access.Session
	if facts.Running() {
		session, err = openSession(ctx, deps, name)
		if err != nil {
			return err
		}
		if _, err := session.Run(ctx, quiesceStop); err != nil {
			return fmt.Errorf("stop docker and containerd on %s: %w", name, err)
		}
		// The services are stopped, but the page cache is not on the disk yet,
		// and the image is taken from the disk.
		if _, err := session.Run(ctx, quiesceFlush); err != nil {
			return fmt.Errorf("flush writes on %s: %w", name, err)
		}
	}

	argv := []string{
		"compute", "images", "create", image,
		deps.Config.ProjectFlag(),
		"--source-disk=" + disk,
		"--source-disk-zone=" + deps.Config.Zone,
		"--labels=" + box.LabelFlag(box.Labels(name)),
	}
	bakeErr := mutate(ctx, deps, record.KindSnapshot, name, argv, "image "+image)

	var restartErr error
	if session != nil {
		// The services come back whatever the capture did: leaving a box with
		// docker stopped because an image failed turns a recoverable failure
		// into an outage.
		if _, err := session.Run(ctx, quiesceStart); err != nil {
			restartErr = fmt.Errorf("restart docker and containerd on %s: %w", name, err)
		}
	}
	if bakeErr != nil {
		return bakeErr
	}
	if restartErr != nil {
		return restartErr
	}
	deps.Printf("image %s", image)
	return nil
}

// own describes one box and refuses to work on a machine devbox did not create.
// Ownership is by label, never by name: a virtual machine someone else made in
// the same project must not be captured by a devbox command that guessed its
// name.
func own(ctx context.Context, deps cli.Deps, name box.Name) (box.Facts, error) {
	out, err := deps.Cloud.Run(ctx,
		"compute", "instances", "describe", name.String(),
		deps.Config.ProjectFlag(), deps.Config.ZoneFlag(), "--format=json")
	if err != nil {
		return box.Facts{}, err
	}
	facts, err := box.Fact(out)
	if err != nil {
		return box.Facts{}, err
	}
	if owned, ok := box.NameFromLabels(facts.Labels); !ok || owned != name {
		return box.Facts{}, fmt.Errorf("instance %s carries no devbox labels; devbox did not create it and will not act on it. If it is yours, label it with devbox-name=%s,devbox-managed=true and devbox will adopt it", name, name)
	}
	return facts, nil
}

// mutate runs one recorded cloud call: the pending record is durable before the
// call, and the outcome is written after it, so a dropped connection can never
// leave a resource devbox cannot name.
func mutate(ctx context.Context, deps cli.Deps, kind string, name box.Name, argv []string, result string) error {
	entry, err := deps.Records.Begin(kind, name.String(), argv)
	if err != nil {
		return fmt.Errorf("record %s: %w", kind, err)
	}
	if _, err := deps.Cloud.Run(ctx, argv...); err != nil {
		if markErr := deps.Records.Unknown(entry, err.Error()); markErr != nil {
			deps.Errorf("could not mark record %s unknown: %v", entry.ID, markErr)
		}
		return err
	}
	if err := deps.Records.Known(entry, result); err != nil {
		return fmt.Errorf("record %s: %w", kind, err)
	}
	return nil
}

// checkImageName enforces the Compute Engine image grammar. A name devbox
// accepts becomes a resource path and a flag value, so a name the API would
// reject is refused here instead of in the middle of a workflow.
func checkImageName(value string) error {
	if value == "" || len(value) > 63 {
		return fmt.Errorf("image name must be 1 to 63 characters")
	}
	for index := range value {
		char := value[index]
		lower := char >= 'a' && char <= 'z'
		digit := char >= '0' && char <= '9'
		if index == 0 && !lower {
			return fmt.Errorf("image name must start with a lowercase letter")
		}
		if !lower && !digit && char != '-' {
			return fmt.Errorf("image name may contain only lowercase letters, digits, and hyphens")
		}
	}
	if strings.HasSuffix(value, "-") {
		return fmt.Errorf("image name may not end with a hyphen")
	}
	return nil
}

// runToolchain prints what a box installs, which is what the renderer puts in
// the startup script.
func runToolchain(_ context.Context, deps cli.Deps, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: devbox toolchain")
	}
	if deps.Config.OmpPackage == "" || deps.Config.OmpVersion == "" {
		return fmt.Errorf("omp_package and omp_version are required to install the harness")
	}
	tools := deps.Config.EffectiveTools(DefaultTools())
	for _, name := range config.SortedTools(tools) {
		deps.Printf("%s %s", name, tools[name])
	}
	deps.Printf("harness %s %s", deps.Config.OmpPackage, deps.Config.OmpVersion)
	return nil
}
