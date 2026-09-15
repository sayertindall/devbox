// Package network owns the VPC pieces a box needs: the SSH firewall rule, the
// Cloud Router and NAT that give a box without an external address its egress.
package network

import "devbox/internal/cli"

// Commands returns the network verbs. Implemented by the lifecycle slice.
func Commands() []cli.Command { return nil }
