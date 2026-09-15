// Package cli holds the command contract every devbox slice implements.
//
// A command receives its dependencies and its own arguments. It never reads a
// global, opens its own configuration, or reaches the network except through
// Deps.Cloud, so every command is exercisable in a test with a recorded cloud.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"devbox/internal/config"
	"devbox/internal/gcloud"
	"devbox/internal/record"
)

// Deps is everything a command may use.
type Deps struct {
	Config     config.Config
	ConfigPath string
	Cloud      gcloud.Executor
	Records    *record.Store
	Out        io.Writer
	Err        io.Writer
	Stdin      io.Reader
	DryRun     bool
}

// Printf writes one line to the command's output.
func (d Deps) Printf(format string, args ...any) {
	fmt.Fprintf(d.Out, format+"\n", args...)
}

// Errorf writes one line to the command's error stream.
func (d Deps) Errorf(format string, args ...any) {
	fmt.Fprintf(d.Err, format+"\n", args...)
}

// FlagSet returns a parser for one command, with errors routed to the command's
// error stream and usage printed by the caller.
func (d Deps) FlagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(d.Err)
	return set
}

// Command is one devbox verb.
type Command struct {
	Name    string
	Summary string
	Usage   string
	Run     func(ctx context.Context, deps Deps, args []string) error
}

// Registry holds every command the binary exposes.
type Registry struct {
	commands []Command
	byName   map[string]Command
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Command{}}
}

// Add registers commands, rejecting a duplicate name so two slices cannot
// silently shadow each other.
func (r *Registry) Add(commands ...Command) {
	for _, command := range commands {
		if command.Name == "" || command.Run == nil {
			panic("devbox: command needs a name and a run function")
		}
		if _, exists := r.byName[command.Name]; exists {
			panic("devbox: duplicate command " + command.Name)
		}
		r.commands = append(r.commands, command)
		r.byName[command.Name] = command
	}
}

// Find returns one command by name.
func (r *Registry) Find(name string) (Command, bool) {
	command, ok := r.byName[name]
	return command, ok
}

// Commands returns every command, ordered by name for help output.
func (r *Registry) Commands() []Command {
	out := append([]Command{}, r.commands...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Help renders the command list.
func (r *Registry) Help() string {
	var lines []string
	width := 0
	for _, command := range r.Commands() {
		if len(command.Name) > width {
			width = len(command.Name)
		}
	}
	for _, command := range r.Commands() {
		lines = append(lines, fmt.Sprintf("  %-*s  %s", width, command.Name, command.Summary))
	}
	return strings.Join(lines, "\n")
}

// Run dispatches one command line.
func (r *Registry) Run(ctx context.Context, deps Deps, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devbox <command> [arguments]\n\ncommands:\n%s", r.Help())
	}
	if args[0] == "help" {
		deps.Printf("commands:\n%s", r.Help())
		return nil
	}
	command, ok := r.Find(args[0])
	if !ok {
		return fmt.Errorf("unknown command %q\n\ncommands:\n%s", args[0], r.Help())
	}
	return command.Run(ctx, deps, args[1:])
}
