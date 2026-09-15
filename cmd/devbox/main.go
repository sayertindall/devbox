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
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
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
	return runWith(ctx, os.Args[1:])
}

// runWith is the whole dispatch path for one command line. It is separate from
// run so a test can drive the real gate, the real registry, and the real
// configuration handling with arguments it chooses.
func runWith(ctx context.Context, argv []string) error {
	global := flag.NewFlagSet("devbox", flag.ContinueOnError)
	global.SetOutput(os.Stderr)
	defaultConfig := "~/.devbox/config.toml"
	if resolved, err := config.DefaultPath(); err == nil {
		defaultConfig = resolved
	}
	configPath := global.String("config", "", "configuration file (default "+defaultConfig+")")
	dryRun := global.Bool("dry-run", false, "print the cloud commands instead of running them")
	project := global.String("project", "", "override the configured project")
	zone := global.String("zone", "", "override the configured zone")
	// ParseGlobals rather than the standard parser: it accepts these flags in any
	// position, and it leaves a lone `--` in the command line, because the words
	// after that marker belong to the box rather than to devbox.
	commandLine, err := cli.ParseGlobals(global, argv)
	showHelp := errors.Is(err, flag.ErrHelp)
	if err != nil && !showHelp {
		return err
	}

	cfg, path, exists, err := config.LoadOptional(*configPath)
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

	deps := cli.Deps{
		Config:     cfg,
		ConfigPath: path,
		Cloud:      gcloud.Runner{Executable: "gcloud", DryRun: *dryRun || os.Getenv("DEVBOX_DRY_RUN") == "1", Output: os.Stdout},
		Out:        os.Stdout,
		Err:        os.Stderr,
		Stdin:      os.Stdin,
		DryRun:     *dryRun || os.Getenv("DEVBOX_DRY_RUN") == "1",
	}
	registry := buildRegistry(deps)

	// Help comes before the configuration gate: asking what a command does is
	// exactly what a person does while the configuration is still blank.
	if requested := helpTarget(commandLine, showHelp); requested != "" || wantsHelp(commandLine) {
		if requested == "" {
			deps.Printf("%s", registry.Help())
			return nil
		}
		text, err := registry.Describe(requested)
		if err != nil {
			return err
		}
		deps.Printf("%s", text)
		return nil
	}

	// The gate runs before the record store is opened, so a command that cannot
	// act leaves nothing behind on the operator's machine.
	if name := firstArgument(commandLine); name != "" {
		configOnly := registry.ConfigOnly(name)
		if !exists && !configOnly {
			return fmt.Errorf("no configuration at %s; create one with: devbox config init --project <project-id>", path)
		}
		if exists && !configOnly {
			if reason := cfg.Explain(); reason != nil {
				return fmt.Errorf("configuration %s cannot create a machine: %w\nfill it in by editing that file (devbox tools edit opens it)", path, reason)
			}
		}
	}

	recordDir, err := config.RecordDir()
	if err != nil {
		return err
	}
	records, err := record.Open(recordDir)
	if err != nil {
		return err
	}
	deps.Records = records

	return buildRegistry(deps).Run(ctx, deps, commandLine)
}

// wantsHelp reports whether the command line asks for help at all.
func wantsHelp(commandLine []string) bool {
	if len(commandLine) == 0 {
		return true
	}
	if commandLine[0] == "help" || commandLine[0] == "-h" || commandLine[0] == "--help" {
		return true
	}
	for _, arg := range commandLine[1:] {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

// helpTarget is the command the operator asked about, or empty for the whole
// list. A bare help flag asks about the whole program.
func helpTarget(commandLine []string, flagHelp bool) string {
	if len(commandLine) == 0 {
		return ""
	}
	switch commandLine[0] {
	case "help":
		if len(commandLine) > 1 {
			return commandLine[1]
		}
		return ""
	case "-h", "--help":
		return ""
	}
	for _, arg := range commandLine[1:] {
		if arg == "-h" || arg == "--help" {
			return commandLine[0]
		}
	}
	if flagHelp {
		return commandLine[0]
	}
	return ""
}

// firstArgument is the command name in an argument list, for the check that
// decides whether a missing configuration is fatal.
func firstArgument(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
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
	registry.Add(builtinCommands(registry)...)
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

// builtinCommands are the commands the binary owns: the operator's own settings
// and the help that names every other command. They work before a project is
// configured, because they are how a project gets configured.
func builtinCommands(registry *cli.Registry) []cli.Command {
	return []cli.Command{
		{
			Name:       "help",
			Summary:    "List the commands, or describe one",
			Usage:      "devbox help [<command>]",
			ConfigOnly: true,
			Help: `With no argument it lists every command. With one it prints that
command's usage line and its flags.`,
			Run: func(_ context.Context, deps cli.Deps, args []string) error {
				if len(args) == 0 {
					deps.Printf("%s", registry.Help())
					return nil
				}
				text, err := registry.Describe(args[0])
				if err != nil {
					return err
				}
				deps.Printf("%s", text)
				return nil
			},
		},
		{
			Name:       "config",
			ConfigOnly: true,
			Summary:    "Show or create the devbox configuration",
			Usage:      "devbox config [init]",
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
					missing, err := config.Init(deps.ConfigPath, deps.Config)
					if err != nil {
						return err
					}
					deps.Printf("wrote %s", deps.ConfigPath)
					if len(missing) > 0 {
						deps.Printf("still to fill in: %s", strings.Join(missing, ", "))
						deps.Printf("edit it with: devbox tools edit (or open that file in any editor)")
						return nil
					}
					deps.Printf("next: devbox network ensure                       firewall, router, and NAT")
					deps.Printf("      devbox bootstrap upload --bucket gs://<bucket>   the startup script")
					deps.Printf("      devbox machine new <name>                   create the box")
					return nil
				default:
					return fmt.Errorf("usage: devbox config [init|show]")
				}
			},
		},
		{
			Name:       "version",
			Summary:    "Print the devbox version",
			Usage:      "devbox version",
			ConfigOnly: true,
			Run: func(_ context.Context, deps cli.Deps, args []string) error {
				deps.Printf("devbox %s", version)
				return nil
			},
		},
	}
}
