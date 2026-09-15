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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
//
// Open reads no cloud state and writes no file: the alias it uses is the one
// devbox ssh-config wrote, and whether that alias reaches the box through an IAP
// tunnel was decided there from the box's own addresses.
func (d Dialer) Open(name box.Name) (Session, error) {
	parsed, err := box.ParseName(string(name))
	if err != nil {
		return nil, err
	}
	return sshSession{
		alias: d.Config.SSHHost(parsed.String()),
		proc:  runProcess,
		out:   d.Out,
		err:   d.Err,
	}, nil
}

// proc runs one local process with separate arguments, never through a shell, so
// a box name, a path, or a remote command can never be read as local shell
// syntax. It is a function type so a test can assert the exact argument vector
// without running ssh or rsync.
type proc func(ctx context.Context, argv []string, in io.Reader, out, errOut io.Writer) error

// runProcess is the real proc.
func runProcess(ctx context.Context, argv []string, in io.Reader, out, errOut io.Writer) error {
	if len(argv) == 0 {
		return errors.New("access: empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errOut
	return cmd.Run()
}

// sshSession reaches one box through the alias devbox owns. Every call is one
// ssh or rsync process: nothing is held open between calls, so a session stays
// valid while a box is stopped and resumed.
type sshSession struct {
	alias string
	proc  proc
	out   io.Writer
	err   io.Writer
}

// Run executes a remote command and returns its combined output.
//
// BatchMode keeps a non-interactive run from stopping on a prompt no caller can
// answer, which is also why the host key must be confirmed once by hand: the
// first devbox ssh to a new box does that, and every later call reuses it.
func (s sshSession) Run(ctx context.Context, command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("access: remote command is required")
	}
	var combined bytes.Buffer
	err := s.proc(ctx, sshArgv(s.alias, command), nil, &combined, &combined)
	out := combined.String()
	if err != nil {
		return out, fmt.Errorf("ssh %s: %w", s.alias, err)
	}
	return out, nil
}

// Upload copies one local path into a remote directory.
func (s sshSession) Upload(ctx context.Context, local, remoteDir string) error {
	if local == "" || remoteDir == "" {
		return errors.New("access: upload needs a local path and a remote directory")
	}
	return s.proc(ctx, uploadArgv(s.alias, local, remoteDir), nil, sink(s.out), sink(s.err))
}

// Download copies one remote path into a local directory.
func (s sshSession) Download(ctx context.Context, remote, localDir string) error {
	if remote == "" || localDir == "" {
		return errors.New("access: download needs a remote path and a local directory")
	}
	return s.proc(ctx, downloadArgv(s.alias, remote, localDir), nil, sink(s.out), sink(s.err))
}

// sink returns a writer for a copy in progress, discarding it when the Dialer was
// built without streams, as it is when only the argument vector matters.
func sink(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// sshArgv is the exact non-interactive ssh invocation. The command travels as one
// argument after --, so the remote shell receives it verbatim instead of as words
// the local shell already split.
func sshArgv(alias, command string) []string {
	return []string{"ssh", "-o", "BatchMode=yes", alias, "--", command}
}

// interactiveArgv is the operator's own connection: the terminal streams straight
// through, and BatchMode is off so the first connection to a box can confirm its
// host key.
func interactiveArgv(alias, command string) []string {
	if command == "" {
		return []string{"ssh", alias}
	}
	return []string{"ssh", alias, "--", command}
}

// uploadArgv copies one local path into a remote directory. --relative keeps the
// path's own shape, so a tree uploaded file by file lands in the directory in the
// same shape it had locally. --delete is never used: an upload may not remove
// anything already on the box.
func uploadArgv(alias, local, remoteDir string) []string {
	return []string{"rsync", "-a", "--relative", local, alias + ":" + directory(remoteDir)}
}

// downloadArgv copies one remote path into a local directory.
func downloadArgv(alias, remote, localDir string) []string {
	return []string{"rsync", "-a", alias + ":" + remote, directory(localDir)}
}

// directory renders an rsync destination as a directory, so a copy into a
// directory that does not exist yet creates it instead of renaming the source to
// the destination's name.
func directory(path string) string {
	return strings.TrimRight(path, "/") + "/"
}
