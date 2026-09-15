// Package agent starts, lists, attaches to, and stops agent sessions running on
// a box, so a long run survives the laptop closing.
package agent

import "devbox/internal/cli"

// Commands returns the agent verbs. Implemented by the agent slice.
func Commands() []cli.Command { return nil }
