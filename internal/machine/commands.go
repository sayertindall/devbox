// Package machine owns the box lifecycle: create, start, stop, suspend, resume,
// list, inspect, and destroy.
package machine

import "devbox/internal/cli"

// Commands returns the lifecycle verbs. Implemented by the lifecycle slice.
func Commands() []cli.Command { return nil }
