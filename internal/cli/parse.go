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
