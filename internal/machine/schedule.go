package machine

import (
	"context"
	"fmt"

	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
)

func policyDescribeArgs(cfg config.Config, policy string) []string {
	return []string{"compute", "resource-policies", "describe", policy, cfg.ProjectFlag(), "--region=" + region(cfg)}
}

// policyCreateArgs defines the stop and start windows. An instance schedule is a
// regional resource policy, so it needs the region the zone belongs to.
func policyCreateArgs(cfg config.Config, policy, start, stop, timezone string) []string {
	return []string{
		"compute", "resource-policies", "create", "instance-schedule", policy,
		cfg.ProjectFlag(),
		"--region=" + region(cfg),
		"--vm-start-schedule=" + start,
		"--vm-stop-schedule=" + stop,
		"--timezone=" + timezone,
	}
}

func policyDeleteArgs(cfg config.Config, policy string) []string {
	return []string{"compute", "resource-policies", "delete", policy, cfg.ProjectFlag(), "--region=" + region(cfg)}
}

func policyAttachArgs(cfg config.Config, name box.Name, policy string) []string {
	return append(instanceArgs(cfg, "add-resource-policies", name.String()), "--resource-policies="+policy)
}

func policyDetachArgs(cfg config.Config, name box.Name, policy string) []string {
	return append(instanceArgs(cfg, "remove-resource-policies", name.String()), "--resource-policies="+policy)
}

// runSchedule attaches or removes the stop and start windows of one box. The
// policy is created when it is missing and attached either way, so running the
// command twice leaves the box with exactly one schedule.
func runSchedule(ctx context.Context, deps cli.Deps, args []string) error {
	set := deps.FlagSet("schedule")
	start := set.String("start", "", "start window, HH:MM in the policy timezone")
	stop := set.String("stop", "", "stop window, HH:MM in the policy timezone")
	timezone := set.String("timezone", "UTC", "IANA timezone the windows are expressed in")
	remove := set.Bool("remove", false, "detach the schedule and delete the policy")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("usage: devbox machine schedule <name> [--start=HH:MM --stop=HH:MM] [--timezone=ZONE] [--remove]")
	}
	name, err := box.ParseName(set.Arg(0))
	if err != nil {
		return err
	}
	if _, err := own(ctx, deps, name); err != nil {
		return err
	}
	policy := schedulePolicy(name)

	if *remove {
		// A box that never carried the schedule, or a schedule already deleted,
		// is the state the operator asked for, so a missing object is not an error.
		if _, err := deps.Cloud.Run(ctx, policyDetachArgs(deps.Config, name, policy)...); err != nil && !gcloud.Missing(err) {
			return err
		}
		if _, err := deps.Cloud.Run(ctx, policyDeleteArgs(deps.Config, policy)...); err != nil && !gcloud.Missing(err) {
			return err
		}
		deps.Printf("removed schedule %s from %s", policy, name)
		return nil
	}

	if *start == "" || *stop == "" {
		return fmt.Errorf("usage: devbox machine schedule <name> --start=HH:MM --stop=HH:MM [--timezone=ZONE]")
	}
	present, err := exists(ctx, deps, policyDescribeArgs(deps.Config, policy))
	if err != nil {
		return err
	}
	if present {
		deps.Printf("schedule %s already exists", policy)
	} else {
		if _, err := deps.Cloud.Run(ctx, policyCreateArgs(deps.Config, policy, *start, *stop, *timezone)...); err != nil {
			return err
		}
		deps.Printf("created schedule %s", policy)
	}
	if _, err := deps.Cloud.Run(ctx, policyAttachArgs(deps.Config, name, policy)...); err != nil {
		return err
	}
	deps.Printf("attached schedule %s to %s", policy, name)
	return nil
}
