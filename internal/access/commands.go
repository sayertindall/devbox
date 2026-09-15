// Package access owns reaching the box: the managed SSH configuration block,
// the IAP tunnel, scp and rsync, port forwarding, the Ghostty terminfo entry,
// and the editor URLs (Zed, VS Code) that reuse the same host alias.
package access

import "devbox/internal/cli"

// Commands returns the access verbs. Implemented by the access slice.
func Commands() []cli.Command { return nil }
