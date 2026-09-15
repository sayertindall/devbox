// Package tree moves a working tree between the local machine and a box under a
// positive allowlist: what is transferred is exactly what the manifest declares,
// so secrets and excluded paths cannot leave the machine by accident.
package tree

import "devbox/internal/cli"

// Commands returns the tree verbs. Implemented by the tree slice.
func Commands() []cli.Command { return nil }
