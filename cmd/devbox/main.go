// Command devbox manages a personal cloud development machine: it creates the
// machine from a pinned image, bootstraps a full toolchain, reaches it over SSH
// (through an IAP tunnel when it has no external address), snapshots and forks
// it, and moves trees to and from it.
//
// The binary is wiring only. Each slice lives in its own package and exposes
// Commands(), so a command can be read, tested, and changed without touching the
// dispatcher.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"devbox/internal/access"
	"devbox/internal/agent"
	"devbox/internal/bootstrap"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
	"devbox/internal/machine"
	"devbox/internal/network"
	"devbox/internal/reconcile"
	"devbox/internal/record"
	"devbox/internal/tools"
	"devbox/internal/tree"
)

// version is the CLI version, reported by devbox version.
const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devbox: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	global := flag.NewFlagSet("devbox", flag.ContinueOnError)
	global.SetOutput(os.Stderr)
	configPath := global.String("config", "", "configuration file (default ~/.config/devbox/config.json)")
	dryRun := global.Bool("dry-run", false, "print the cloud commands instead of running them")
	project := global.String("project", "", "override the configured project")
	zone := global.String("zone", "", "override the configured zone")
	if err := global.Parse(os.Args[1:]); err != nil {
		return err
	}

	cfg, path, err := config.LoadOrDefault(*configPath)
	if err != nil {
		return err
	}
	if *project != "" {
		cfg.Project = *project
	}
	if *zone != "" {
		cfg.Zone = *zone
		cfg.Region = config.RegionFromZone(*zone)
	}

	recordDir, err := config.RecordDir()
	if err != nil {
		return err
	}
	records, err := record.Open(recordDir)
	if err != nil {
		return err
	}

	deps := cli.Deps{
		Config:     cfg,
		ConfigPath: path,
		Cloud:      gcloud.Runner{Executable: "gcloud", DryRun: *dryRun || os.Getenv("DEVBOX_DRY_RUN") == "1", Output: os.Stdout},
		Records:    records,
		Out:        os.Stdout,
		Err:        os.Stderr,
		Stdin:      os.Stdin,
		DryRun:     *dryRun || os.Getenv("DEVBOX_DRY_RUN") == "1",
	}

	return buildRegistry(deps).Run(ctx, deps, global.Args())
}

// buildRegistry wires every slice's commands into one dispatcher. It is a
// function so the end-to-end test drives the same wiring the binary does,
// including the duplicate-name check.
func buildRegistry(deps cli.Deps) *cli.Registry {
	registry := cli.NewRegistry()
	registry.Add(machine.Commands()...)
	registry.Add(network.Commands()...)
	registry.Add(access.Commands()...)
	registry.Add(bootstrap.Commands()...)
	registry.Add(tree.Commands()...)
	registry.Add(agent.Commands()...)
	registry.Add(tools.CommandsWith(tools.Mise{}, tools.Npm{}, boxDialer{deps: deps})...)
	registry.Add(reconcile.Commands()...)
	registry.Add(configCommands()...)
	return registry
}

// boxDialer adapts the access package's session to the narrow interface the
// tools slice declares, so tool management does not import the SSH layer.
type boxDialer struct{ deps cli.Deps }

func (d boxDialer) Open(ctx context.Context, name box.Name) (tools.Session, error) {
	session, err := access.Dialer{Config: d.deps.Config, Cloud: d.deps.Cloud, Out: d.deps.Out, Err: d.deps.Err, Stdin: d.deps.Stdin}.Open(ctx, name)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// configCommands are the operator's own settings: they describe and create the
// configuration file every other command reads.
func configCommands() []cli.Command {
	return []cli.Command{
		{
			Name:    "config",
			Summary: "Show or create the devbox configuration",
			Usage:   "devbox config [init]",
			Run: func(_ context.Context, deps cli.Deps, args []string) error {
				action := "show"
				if len(args) > 0 {
					action = args[0]
				}
				switch action {
				case "show":
					deps.Printf("config %s", deps.ConfigPath)
					deps.Printf("project %s zone %s machine %s", deps.Config.Project, deps.Config.Zone, deps.Config.MachineType)
					deps.Printf("image %s/%s boot %dGB %s data %dGB %s", deps.Config.ImageProject, deps.Config.ImageFamily, deps.Config.BootDiskGB, deps.Config.BootDiskType, deps.Config.DataDiskGB, deps.Config.DataDiskType)
					deps.Printf("ssh prefix %s user %s external_ip %t", deps.Config.SSHPrefix, deps.Config.RemoteUser, deps.Config.ExternalIP)
					if deps.Config.BootstrapURL == "" {
						deps.Printf("bootstrap url (unset)")
					} else {
						deps.Printf("bootstrap %s", deps.Config.BootstrapURL)
					}
					return nil
				case "init":
					if deps.Config.Project == "" {
						return fmt.Errorf("config init needs a project: pass --project or set project in %s", deps.ConfigPath)
					}
					if err := config.Save(deps.ConfigPath, deps.Config); err != nil {
						return err
					}
					deps.Printf("wrote %s", deps.ConfigPath)
					return nil
				default:
					return fmt.Errorf("usage: devbox config [init|show]")
				}
			},
		},
		{
			Name:    "version",
			Summary: "Print the devbox version",
			Usage:   "devbox version",
			Run: func(_ context.Context, deps cli.Deps, args []string) error {
				deps.Printf("devbox %s", version)
				return nil
			},
		},
	}
}
