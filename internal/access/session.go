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
	"time"

	"devbox/internal/box"
	"devbox/internal/config"
	"devbox/internal/gcloud"
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
	// Cloud, when set, lets Open refresh the managed SSH entry from what the
	// cloud says the box is right now, so a caller never has to remember to run
	// ssh-config after a start or a resume.
	Cloud gcloud.Executor
	// DryRun keeps Open from touching the operator's files, because a rehearsal
	// that rewrote the SSH configuration would be a mutation like any other.
	DryRun bool
	Out    io.Writer
	Err    io.Writer
	Stdin  io.Reader
}

// Open returns a Session for one box.
//
// With a cloud executor the box's Host entry is refreshed from what the cloud
// says the box is right now: the address a box answers on is not fixed, because
// one that has an external address can be given a different one across a stop and
// a start, and one without an address keeps the entry it already has, so a second
// Open of an unchanged box rewrites nothing. Without a cloud executor Open writes
// no file and uses whatever entry the operator already has.
//
// The dialer's output streams carry a copy in progress; the input stream belongs
// to the verbs that hand the operator's terminal to ssh, which is why a captured
// run sends none.
func (d Dialer) Open(ctx context.Context, name box.Name) (Session, error) {
	parsed, err := box.ParseName(name.String())
	if err != nil {
		return nil, err
	}
	if d.Cloud != nil {
		if err := d.refreshEntry(ctx, parsed); err != nil {
			return nil, err
		}
	}
	return sshSession{
		alias: d.Config.SSHHost(parsed.String()),
		proc:  runProcess,
		out:   d.Out,
		err:   d.Err,
	}, nil
}

// describeFailure turns a missing box into the two things the operator can do
// about it, rather than repeating the cloud CLI's own wording.
func describeFailure(cfg config.Config, name box.Name, err error) error {
	if gcloud.Missing(err) || strings.Contains(err.Error(), "was not found") {
		return fmt.Errorf("box %s was not found in project %s zone %s; see devbox machine list, or create it with devbox machine new %s",
			name, cfg.Project, cfg.Zone, name)
	}
	return fmt.Errorf("describe %s: %w", name, err)
}

// refreshEntry writes the managed Host entry from the box's current addresses.
//
// The entry has to be current rather than merely present: a box with no external
// address is reached through the IAP tunnel, which resolves the instance name and
// survives a stop and a start, while a box with an address can come back on a
// different one.
func (d Dialer) refreshEntry(ctx context.Context, name box.Name) error {
	if d.DryRun {
		if d.Out != nil {
			fmt.Fprintf(d.Out, "would refresh the Host entry for %s in the SSH configuration\n", d.Config.SSHHost(name.String()))
		}
		return nil
	}
	path, err := sshConfigPath()
	if err != nil {
		return err
	}
	out, err := d.Cloud.Run(ctx, describeArgs(d.Config, name)...)
	if err != nil {
		return describeFailure(d.Config, name, err)
	}
	facts, err := box.Fact(out)
	if err != nil {
		return describeFailure(d.Config, name, err)
	}
	if _, err := writeSSHBlock(path, d.Config, name, facts.ExternalIP()); err != nil {
		return err
	}
	return nil
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
// answer: a key that is not authorized yet fails instead of waiting for a
// password. The managed entry's accept-new setting records the box's host key on
// the first connection, so no run stops on a host key question either.
func (s sshSession) Run(ctx context.Context, command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("access: remote command is required")
	}
	deadline := time.Now().Add(connectWait)
	for attempt := 0; ; attempt++ {
		var combined bytes.Buffer
		err := s.proc(ctx, sshArgv(s.alias, command), nil, &combined, &combined)
		out := combined.String()
		if err == nil {
			return out, nil
		}
		if !transient(out) || time.Now().After(deadline) {
			return out, fmt.Errorf("ssh %s: %w", s.alias, err)
		}
		if attempt == 0 && s.err != nil {
			fmt.Fprintf(s.err, "%s is not answering yet; waiting for it to finish booting\n", s.alias)
		}
		select {
		case <-ctx.Done():
			return out, fmt.Errorf("ssh %s: %w", s.alias, err)
		case <-time.After(connectStep):
		}
	}
}

// connectWait is how long the first connection waits for a box that is still
// booting. A box reports RUNNING before its ssh daemon listens, and the tunnel in
// front of it reports that as a refused backend, so a connection made right after
// a start or a create fails for a reason that clears on its own.
const (
	connectWait = 90 * time.Second
	connectStep = 3 * time.Second
)

// transient reports whether a failed connection failed because the box was not
// ready yet. The phrases are the tunnel's and ssh's own words for a closed port. A
// refused key is deliberately absent: that never clears by waiting.
func transient(output string) bool {
	for _, phrase := range []string{
		"failed to connect to backend",
		"Connection refused",
		"Connection closed",
		"Connection timed out",
		"No route to host",
	} {
		if strings.Contains(output, phrase) {
			return true
		}
	}
	return false
}

// boxStateCommand reports whether the box finished its bootstrap, so a session
// that opens onto a box still installing can say so rather than leaving the
// operator to wonder why half the toolchain is missing.
func boxStateCommand() string {
	return fmt.Sprintf("if [ -f %s ]; then echo %s; elif [ -f %s ]; then echo %s; tail -n 1 %s; else echo %s; fi",
		box.BootstrapStamp, boxReady, box.BootstrapFailed, boxFailed, box.BootstrapLog, boxBooting)
}

// The three states boxStateCommand reports, and the notice each one earns. A
// failed install is reported as failed with the line that caused it, because a
// box that never becomes ready has to say so rather than look slow forever.
const (
	boxReady   = "ready"
	boxBooting = "bootstrapping"
	boxFailed  = "failed"
)

// bootNotice is what a session says about a box that has not finished building
// itself. For a failure it carries the bootstrap's own last line, which is the
// difference between the operator reading a cause and reading a serial console.
func bootNotice(cfg config.Config, name box.Name, state string) string {
	lines := strings.Split(strings.TrimSpace(state), "\n")
	switch lines[0] {
	case boxFailed:
		detail := "no reason recorded"
		if len(lines) > 1 && strings.TrimSpace(lines[1]) != "" {
			detail = strings.TrimSpace(lines[1])
		}
		return fmt.Sprintf("the bootstrap failed on %s: %s\nthe full log is %s on the box", name, detail, box.BootstrapLog)
	default:
		return fmt.Sprintf("%s is still bootstrapping; follow it with: devbox ssh %s -- tail -f %s",
			cfg.SSHHost(name.String()), name, box.BootstrapLog)
	}
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
