// Package network owns the VPC pieces a box needs: the SSH firewall rule, the
// Cloud Router and NAT that give a box without an external address its egress.
package network

import (
	"context"
	"fmt"

	"devbox/internal/cli"
)

// Commands returns the network verbs. The registry dispatches on a single word,
// so the two verbs live behind one command and are selected by its argument.
func Commands() []cli.Command {
	return []cli.Command{
		{
			Name:    "network",
			Summary: "Ensure or show the firewall, router, and NAT a box needs",
			Usage:   "devbox network <ensure|show>",
			Run:     runNetwork,
		},
	}
}

func runNetwork(ctx context.Context, deps cli.Deps, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: devbox network <ensure|show>")
	}
	switch args[0] {
	case "ensure":
		return ensure(ctx, deps)
	case "show":
		return show(ctx, deps)
	default:
		return fmt.Errorf("unknown network verb %q\n\nusage: devbox network <ensure|show>", args[0])
	}
}
