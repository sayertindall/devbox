package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"devbox/internal/bootstrap"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
)

// Session is the part of a box session this package needs. Declaring it here
// keeps tool management independent of how a session is opened, so the access
// slice can change without touching this one.
type Session interface {
	Run(ctx context.Context, command string) (string, error)
}

// Dialer opens a session on a box. The binary injects one that uses the managed
// SSH configuration.
type Dialer interface {
	Open(ctx context.Context, name box.Name) (Session, error)
}

// Commands returns the toolchain verbs.
func Commands() []cli.Command { return CommandsWith(Mise{}, Npm{}, nil) }

// CommandsWith returns the verbs with injected resolvers and dialer, which is how
// tests run them without mise, npm, or a box.
func CommandsWith(mise, npm Resolver, dialer Dialer) []cli.Command {
	return []cli.Command{{
		Name:    "tools",
		Summary: "Add, remove, update, and apply the toolchain pins",
		Usage:   "devbox tools <list|add|remove|update|outdated|apply|edit>",
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			return run(ctx, deps, args, mise, npm, dialer)
		},
	}}
}

func run(ctx context.Context, deps cli.Deps, args []string, mise, npm Resolver, dialer Dialer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devbox tools <list|add|remove|update|outdated|apply|edit>")
	}
	action, rest := args[0], args[1:]
	switch action {
	case "list":
		return list(deps)
	case "add":
		return add(ctx, deps, rest, mise)
	case "remove":
		return remove(deps, rest)
	case "update":
		return update(ctx, deps, rest, mise, npm)
	case "outdated":
		return outdated(ctx, deps, mise, npm)
	case "apply":
		return apply(ctx, deps, rest, dialer)
	case "edit":
		return edit(deps)
	default:
		return fmt.Errorf("unknown tools action %q", action)
	}
}

// pins is the toolchain a box installs, and whether the file declares it.
func pins(deps cli.Deps) (map[string]string, bool) {
	effective := deps.Config.EffectiveTools(bootstrap.DefaultTools())
	return effective, len(deps.Config.Tools) == 0
}

func list(deps cli.Deps) error {
	effective, defaults := pins(deps)
	deps.Printf("toolchain from %s", deps.ConfigPath)
	if defaults {
		deps.Printf("no [tools] pins declared, showing the built-in toolchain")
	}
	names := make([]string, 0, len(effective))
	for name := range effective {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		deps.Printf("  %-18s %s", name, effective[name])
	}
	deps.Printf("  %-18s %s %s", "agent harness", deps.Config.EffectiveOmpPackage(), deps.Config.EffectiveOmpVersion())
	return nil
}

func add(ctx context.Context, deps cli.Deps, args []string, mise Resolver) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: devbox tools add <tool>[@version]")
	}
	name, version := splitSpec(args[0])
	if name == "" {
		return fmt.Errorf("usage: devbox tools add <tool>[@version]")
	}
	if version == "" {
		resolved, err := mise.Latest(ctx, name)
		if err != nil {
			return err
		}
		version = resolved
	}
	// Declaring one pin makes the file authoritative, so the built-in pins are
	// materialized first. Otherwise adding a tool would silently drop the rest of
	// the toolchain from the next box.
	if _, defaults := pins(deps); defaults {
		builtins := bootstrap.DefaultTools()
		for _, builtin := range config.SortedTools(builtins) {
			if err := config.SetTool(deps.ConfigPath, builtin, builtins[builtin]); err != nil {
				return err
			}
		}
		deps.Printf("wrote the %d built-in pins into %s so the file lists the whole toolchain", len(builtins), deps.ConfigPath)
	}
	if err := config.SetTool(deps.ConfigPath, name, version); err != nil {
		return err
	}
	deps.Printf("pinned %s %s", name, version)
	deps.Printf("next: devbox tools apply <box> to converge a running box, or reboot it")
	return nil
}

func remove(deps cli.Deps, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: devbox tools remove <tool>")
	}
	if err := config.RemoveTool(deps.ConfigPath, args[0]); err != nil {
		return err
	}
	deps.Printf("removed %s", args[0])
	return nil
}

func update(ctx context.Context, deps cli.Deps, args []string, mise, npm Resolver) error {
	effective, defaults := pins(deps)
	if defaults {
		deps.Printf("no [tools] pins declared; the built-in toolchain is in use")
		deps.Printf("add one with: devbox tools add <tool>")
	} else {
		targets := args
		if len(targets) == 0 {
			targets = config.SortedTools(effective)
		}
		changed := 0
		for _, name := range targets {
			current := effective[name]
			latest, err := mise.Latest(ctx, name)
			if err != nil {
				return err
			}
			if latest == current {
				deps.Printf("%s %s is current", name, current)
				continue
			}
			if err := config.SetTool(deps.ConfigPath, name, latest); err != nil {
				return err
			}
			deps.Printf("%s %s -> %s", name, current, latest)
			changed++
		}
		if changed == 0 {
			deps.Printf("all %d pins are current", len(targets))
		}
	}
	harness := deps.Config.EffectiveOmpPackage()
	latest, err := npm.Latest(ctx, harness)
	if err != nil {
		deps.Errorf("harness: %v", err)
		return nil
	}
	if latest == deps.Config.EffectiveOmpVersion() {
		deps.Printf("harness %s is current", latest)
		return nil
	}
	if err := config.SetValue(deps.ConfigPath, "omp_version", latest); err != nil {
		return err
	}
	deps.Printf("harness %s -> %s", deps.Config.EffectiveOmpVersion(), latest)
	return nil
}

func outdated(ctx context.Context, deps cli.Deps, mise, npm Resolver) error {
	effective, defaults := pins(deps)
	if defaults {
		deps.Printf("no [tools] pins declared; nothing to compare")
		return nil
	}
	drift := 0
	for _, name := range config.SortedTools(effective) {
		latest, err := mise.Latest(ctx, name)
		if err != nil {
			return err
		}
		if latest != effective[name] {
			deps.Printf("%-18s %s -> %s", name, effective[name], latest)
			drift++
		}
	}
	harness := deps.Config.EffectiveOmpPackage()
	if latest, err := npm.Latest(ctx, harness); err == nil {
		if latest != deps.Config.EffectiveOmpVersion() {
			deps.Printf("%-18s %s -> %s", "agent harness", deps.Config.EffectiveOmpVersion(), latest)
			drift++
		}
	} else {
		deps.Errorf("harness: %v", err)
	}
	if drift == 0 {
		deps.Printf("every pin is current")
		return nil
	}
	deps.Printf("%d pins behind; run devbox tools update", drift)
	return nil
}

func apply(ctx context.Context, deps cli.Deps, args []string, dialer Dialer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: devbox tools apply <box>")
	}
	name, err := box.ParseName(args[0])
	if err != nil {
		return err
	}
	script := convergeScript(deps)
	if deps.DryRun {
		deps.Printf("would run on %s:", name)
		deps.Printf("%s", script)
		return nil
	}
	if dialer == nil {
		return fmt.Errorf("no box dialer is wired into this build")
	}
	session, err := dialer.Open(ctx, name)
	if err != nil {
		return err
	}
	output, err := session.Run(ctx, script)
	if output != "" {
		deps.Printf("%s", strings.TrimRight(output, "\n"))
	}
	if err != nil {
		return fmt.Errorf("converge %s: %w", name, err)
	}
	deps.Printf("converged %s", name)
	return nil
}

// convergeScript is the remote command that brings a running box to the pinned
// toolchain. The same pins feed the startup script a new box runs, so the two
// paths cannot disagree about the toolchain.
func convergeScript(deps cli.Deps) string {
	effective := deps.Config.EffectiveTools(bootstrap.DefaultTools())
	var lines []string
	lines = append(lines, "set -eu")
	for _, name := range config.SortedTools(effective) {
		lines = append(lines, "mise use -g "+shellQuote(name+"@"+effective[name]))
	}
	lines = append(lines, "mise install")
	harness := deps.Config.EffectiveOmpPackage() + "@" + deps.Config.EffectiveOmpVersion()
	lines = append(lines, "npm install -g "+shellQuote(harness))
	return strings.Join(lines, "\n") + "\n"
}

func edit(deps cli.Deps) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		return fmt.Errorf("EDITOR is not set; edit %s directly", deps.ConfigPath)
	}
	command := exec.Command(editor, deps.ConfigPath)
	command.Stdin = deps.Stdin
	command.Stdout = deps.Out
	command.Stderr = deps.Err
	return command.Run()
}

// splitSpec splits tool@version on the last at-sign, so a backend-qualified name
// such as npm:@scope/pkg keeps its prefix.
func splitSpec(spec string) (string, string) {
	index := strings.LastIndex(spec, "@")
	if index <= 0 || index == len(spec)-1 {
		return spec, ""
	}
	return spec[:index], spec[index+1:]
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
