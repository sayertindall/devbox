// Package bootstrap renders and publishes the startup script that turns a stock
// Debian image into a working box, bakes a reusable image from a configured box,
// and reports what the box installed.
package bootstrap

import "devbox/internal/cli"

// Commands returns the bootstrap verbs. Implemented by the bootstrap slice.
func Commands() []cli.Command { return nil }
