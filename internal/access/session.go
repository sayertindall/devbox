// Package access owns reaching the box: the managed SSH configuration block, the
// IAP tunnel, scp and rsync, port forwarding, the Ghostty terminfo entry, and the
// editor URLs (Zed, VS Code) that reuse the same host alias.
//
// Session is the cross-slice contract: the machine slice runs quiesce commands
// through it before a snapshot, the tree slice copies through it, and the agent
// slice starts sessions with it. Their tests use a recording Session, so no test
// needs a live box.
package access

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"devbox/internal/box"
	"devbox/internal/config"
)

// Session runs commands and moves files on one box.
type Session interface {
	// Run executes a shell command on the box and returns its combined output.
	Run(ctx context.Context, command string) (string, error)
	// Upload copies one local path into a remote directory.
	Upload(ctx context.Context, local, remoteDir string) error
	// Download copies one remote path into a local directory.
	Download(ctx context.Context, remote, localDir string) error
}

// Recording is a Session test double: it records commands and file moves and
// answers scripted output.
type Recording struct {
	Commands  []string
	Uploads   [][2]string
	Downloads [][2]string
	// Reply, when set, returns the output for a command.
	Reply func(command string) (string, error)
	// Fail, when set, fails any command containing the fragment.
	Fail string
}

// Run records a command and returns the scripted reply.
func (r *Recording) Run(_ context.Context, command string) (string, error) {
	r.Commands = append(r.Commands, command)
	if r.Fail != "" && strings.Contains(command, r.Fail) {
		return "", fmt.Errorf("command failed: %s", command)
	}
	if r.Reply != nil {
		return r.Reply(command)
	}
	return "", nil
}

// Upload records a local to remote copy.
func (r *Recording) Upload(_ context.Context, local, remoteDir string) error {
	r.Uploads = append(r.Uploads, [2]string{local, remoteDir})
	return nil
}

// Download records a remote to local copy.
func (r *Recording) Download(_ context.Context, remote, localDir string) error {
	r.Downloads = append(r.Downloads, [2]string{remote, localDir})
	return nil
}

// Dialer opens a Session for one box. The real implementation shells out to ssh
// with the alias devbox writes into the SSH configuration.
type Dialer struct {
	Config config.Config
	Out    io.Writer
	Err    io.Writer
	Stdin  io.Reader
}

// Open returns a Session for one box.
func (d Dialer) Open(name box.Name) (Session, error) {
	if name == "" {
		return nil, errors.New("box name is required")
	}
	return nil, errors.New("access: ssh sessions are implemented by the access slice")
}
