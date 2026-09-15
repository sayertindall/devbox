package cli

import (
	"flag"
	"fmt"
	"strings"
)

// Parse accepts flags in any position, which the standard flag package does not.
//
// flag stops at the first non-flag argument, so `devbox tools apply dev --dry-run`
// would silently treat the flag as a value. devbox commands read naturally in that
// order, so flags are collected first, parsed once, and the remaining arguments are
// returned in the order they were written. A lone `--` ends flag collection, so a
// remote command can follow the box name without being mistaken for flags.
func Parse(set *flag.FlagSet, args []string) ([]string, error) {
	var flagArgs []string
	var positional []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positional = append(positional, args[index+1:]...)
			break
		}
		if len(arg) < 2 || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		if strings.Contains(arg, "=") {
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if definition := set.Lookup(name); definition != nil && isBoolFlag(definition) {
			continue
		}
		if index+1 < len(args) {
			flagArgs = append(flagArgs, args[index+1])
			index++
		}
	}
	if err := set.Parse(flagArgs); err != nil {
		return nil, err
	}
	if set.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q", set.Arg(0))
	}
	return positional, nil
}

// isBoolFlag reports whether a flag can stand alone, which decides whether the
// next argument is its value.
func isBoolFlag(definition *flag.Flag) bool {
	type boolFlag interface{ IsBoolFlag() bool }
	value, ok := definition.Value.(boolFlag)
	return ok && value.IsBoolFlag()
}

// ParseGlobals collects the binary's own flags from anywhere in the argument
// list and returns the command line the dispatcher should run.
//
// It differs from Parse in one way that matters: a lone `--` is preserved in the
// returned command line instead of being consumed. A remote command is written
// `devbox exec dev -- git log --oneline`, and the marker is part of what the
// command needs to see, because the words after it belong to the box, not to
// devbox.
func ParseGlobals(set *flag.FlagSet, args []string) ([]string, error) {
	var flagArgs []string
	var command []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			command = append(command, args[index:]...)
			break
		}
		if len(arg) < 2 || !strings.HasPrefix(arg, "-") {
			command = append(command, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if set.Lookup(name) == nil {
			// Not a flag of this program: it belongs to the command, and passing
			// it through unchanged is what lets `devbox bootstrap upload
			// --bucket gs://b` reach the verb that defines --bucket.
			command = append(command, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		if strings.Contains(arg, "=") || isBoolFlag(set.Lookup(name)) {
			continue
		}
		if index+1 < len(args) {
			flagArgs = append(flagArgs, args[index+1])
			index++
		}
	}
	// The command line is returned even when parsing fails: `devbox machine
	// --help` fails parsing with flag.ErrHelp, and the caller still needs to know
	// which command the operator asked about.
	if err := set.Parse(flagArgs); err != nil {
		return command, err
	}
	if set.NArg() != 0 {
		return command, fmt.Errorf("unexpected argument %q", set.Arg(0))
	}
	return command, nil
}
