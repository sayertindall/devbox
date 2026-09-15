package machine

import (
	"context"
	"fmt"
	"strings"

	"devbox/internal/cli"
)

// machineUsage derives the verb list from the table below, so help, the
// unknown-verb error, and the verbs themselves cannot drift apart.
var machineUsage = "devbox machine <" + strings.Join(verbNames(), "|") + ">"

// machineHelp is the flag detail for the verbs that take any.
const machineHelp = `flags:
  new        --no-bootstrap        create the box without a startup script
             --schedule            attach this box's start and stop windows
  schedule   --start=HH:MM --stop=HH:MM --timezone=ZONE   create the windows
             --remove              detach and delete the policy
  destroy    --confirm=<name>      required; the box name again
             --with-images         also delete the machine images labeled for it
  snapshot   stops docker and containerd first, then restarts them`

// verbNames is every machine verb name in table order.
func verbNames() []string {
	names := make([]string, 0, len(verbs))
	for _, item := range verbs {
		names = append(names, item.name)
	}
	return names
}

// verb is one machine action with its help text and its implementation.
type verb struct {
	name    string
	summary string
	usage   string
	run     func(context.Context, cli.Deps, []string) error
}

// verbs is every machine action. The slice registers one top-level command, so
// no verb of this slice can collide with a verb of another slice.
var verbs = []verb{
	{"new", "Create a box from the configured image", "devbox machine new <name> [--no-bootstrap] [--schedule]", runNew},
	{"start", "Start a stopped box", "devbox machine start <name>", state("start")},
	{"stop", "Stop a box, keeping its disks", "devbox machine stop <name>", state("stop")},
	{"suspend", "Suspend a box, writing its memory to disk", "devbox machine suspend <name>", state("suspend")},
	{"resume", "Resume a suspended box", "devbox machine resume <name>", state("resume")},
	{"list", "List the boxes devbox manages", "devbox machine list", runList},
	{"show", "Show one box, its address, and its unresolved records", "devbox machine show <name>", runShow},
	{"snapshot", "Snapshot a box's data disk with its services stopped", "devbox machine snapshot <name>", runSnapshot},
	{"fork", "Fork a box into a new box through a machine image", "devbox machine fork <name> <new>", runFork},
	{"schedule", "Attach or remove a box's start and stop windows", "devbox machine schedule <name> [--start=HH:MM --stop=HH:MM] [--timezone=ZONE] [--remove]", runSchedule},
	{"destroy", "Delete a box and the resources devbox labeled for it", "devbox machine destroy <name> --confirm=<name> [--with-images]", runDestroy},
}

// Commands returns the machine command.
func Commands() []cli.Command {
	return []cli.Command{{
		Name:    "machine",
		Summary: "Create, inspect, fork, and destroy boxes",
		Usage:   machineUsage,
		Help:    machineHelp,
		Run:     runMachine,
	}}
}

func runMachine(ctx context.Context, deps cli.Deps, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s", machineUsage)
	}
	if args[0] == "help" {
		var lines []string
		for _, item := range verbs {
			lines = append(lines, "  "+item.usage)
		}
		deps.Printf("usage: %s\n%s", machineUsage, strings.Join(lines, "\n"))
		return nil
	}
	for _, item := range verbs {
		if item.name == args[0] {
			return item.run(ctx, deps, args[1:])
		}
	}
	return fmt.Errorf("unknown machine verb %q\n\nusage: %s", args[0], machineUsage)
}

// state builds one state verb. The four are the same call with a different
// gcloud verb, and none of them deletes anything, so none needs confirmation.
func state(name string) func(context.Context, cli.Deps, []string) error {
	return func(ctx context.Context, deps cli.Deps, args []string) error {
		boxName, err := oneName(name, args)
		if err != nil {
			return err
		}
		if deps.DryRun {
			// A rehearsal has nothing to read: a describe would come back empty and
			// reporting "does not exist" would be a lie about the operator's cloud.
			argv := append(instanceArgs(deps.Config, name, boxName.String()), "--quiet")
			deps.Printf("would run: gcloud %s", strings.Join(argv, " "))
			return nil
		}
		if _, err := own(ctx, deps, boxName); err != nil {
			return err
		}
		// --quiet so an unattended run never blocks on a prompt.
		argv := append(instanceArgs(deps.Config, name, boxName.String()), "--quiet")
		if _, err := deps.Cloud.Run(ctx, argv...); err != nil {
			return err
		}
		deps.Printf("%s %s in %s", name, boxName, deps.Config.Zone)
		return nil
	}
}
