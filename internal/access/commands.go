package access

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
)

// ops is what the access commands need from the environment beyond cli.Deps: a
// session for a box and a local process with the operator's own streams. Tests
// substitute both, so no access command needs a live box or a real ssh.
type ops struct {
	open func(ctx context.Context, deps cli.Deps, name box.Name) (Session, error)
	proc proc
}

// realOps is the production wiring of ops.
func realOps() ops {
	return ops{
		open: func(ctx context.Context, deps cli.Deps, name box.Name) (Session, error) {
			return Dialer{Config: deps.Config, Cloud: deps.Cloud, DryRun: deps.DryRun, Out: deps.Out, Err: deps.Err, Stdin: deps.Stdin}.Open(ctx, name)
		},
		proc: runProcess,
	}
}

// reach opens the box so its Host entry is current, then returns the alias for a
// verb that runs ssh itself. Opening a session is the refresh, and it is the only
// path that knows the box exists: without it ssh would fail on a name that
// resolves nowhere, which is what a missing entry looks like from here.
func reach(ctx context.Context, o ops, deps cli.Deps, name box.Name) (string, error) {
	if _, err := o.open(ctx, deps, name); err != nil {
		return "", err
	}
	return deps.Config.SSHHost(name.String()), nil
}

// Commands returns the access verbs.
func Commands() []cli.Command { return commandSet(realOps()) }

// commandSet builds the verbs against one set of environment operations.
func commandSet(o ops) []cli.Command {
	return []cli.Command{
		opSSHConfig(),
		opSSH(o),
		opExec(o),
		opCopy(o),
		opForward(),
		opTerminfo(o),
		opEditors(),
		opDoctor(o),
	}
}

// opSSHConfig publishes the box's address and credentials as one Host entry in
// the operator's SSH configuration, which is how every other tool reaches the box
// without knowing anything about tunnels.
func opSSHConfig() cli.Command {
	const usage = "devbox ssh-config <name>"
	return cli.Command{
		Name:    "ssh-config",
		Summary: "Write the box's Host entry into the operator's SSH configuration",
		Usage:   usage,
		Help:    "Only the block between the devbox markers is rewritten; the rest of the file is yours. Every session refreshes it anyway, so this is for readers that do not go through devbox.",
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			positional, err := cli.Parse(deps.FlagSet("ssh-config"), args)
			if err != nil {
				return parseError(err, usage)
			}
			name, err := onlyBox(positional, usage)
			if err != nil {
				return err
			}
			path, err := sshConfigPath()
			if err != nil {
				return err
			}
			out, err := deps.Cloud.Run(ctx, describeArgs(deps.Config, name)...)
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) == "" {
				if deps.DryRun {
					deps.Printf("not writing %s: a dry run does not read the box's addresses", path)
					return gcloud.ErrDryRun
				}
				return fmt.Errorf("box %s was not found in zone %s", name, deps.Config.Zone)
			}
			facts, err := box.Fact(out)
			if err != nil {
				return fmt.Errorf("read box %s: %w", name, err)
			}
			externalIP := facts.ExternalIP()
			block, err := writeSSHBlock(path, deps.Config, name, externalIP)
			if err != nil {
				return err
			}
			state := "updated"
			if !block.Changed {
				state = "unchanged"
			}
			deps.Printf("%s %s", state, block.Path)
			for _, line := range block.Lines {
				deps.Printf("%s", line)
			}
			if externalIP == "" {
				deps.Printf("connections to %s run through an IAP tunnel", name)
			}
			return nil
		},
	}
}

// describeArgs reads one instance back. It is the only way devbox learns which of
// the two SSH entries a box needs, because a box with an external address is
// reached directly and a box without one needs the tunnel. devbox ssh-config and
// every Open that has a cloud executor read a box this way, so there is one
// vector to keep right.
func describeArgs(cfg config.Config, name box.Name) []string {
	return []string{
		"compute", "instances", "describe", name.String(),
		cfg.ZoneFlag(), cfg.ProjectFlag(), "--format=json",
	}
}

// dryRun stops an action that needs the box, printing the exact command it would
// run so a rehearsal stays useful: the operator can paste it by hand. Every verb
// that would connect or copy asks this first.
func dryRun(deps cli.Deps, argv ...string) bool {
	if !deps.DryRun {
		return false
	}
	if len(argv) > 0 {
		deps.Printf("would run: %s", strings.Join(argv, " "))
	}
	return true
}

// opSSH opens the operator's own connection: a shell, or one command with the
// terminal streaming through so a long command can be watched and interrupted. A
// remote command that has flags of its own follows --, which is what keeps the
// local parser from claiming them.
func opSSH(o ops) cli.Command {
	const usage = "devbox ssh <name> [-- command]"
	return cli.Command{
		Name:    "ssh",
		Summary: "Open a shell on a box, or run one command through it",
		Usage:   usage,
		Help:    "Everything after -- is the remote command, so its own flags reach the box: devbox ssh dev -- tail -n 50 /var/log/devbox-bootstrap.log",
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			positional, err := cli.Parse(deps.FlagSet("ssh"), args)
			if err != nil {
				return parseError(err, usage)
			}
			name, err := firstBox(positional, usage)
			if err != nil {
				return err
			}
			remote := strings.Join(positional[1:], " ")
			alias, err := reach(ctx, o, deps, name)
			if err != nil {
				return err
			}
			argv := interactiveArgv(alias, remote)
			if dryRun(deps, argv...) {
				return nil
			}
			return o.proc(ctx, argv, deps.Stdin, deps.Out, deps.Err)
		},
	}
}

// opExec runs one command and prints its output, which is what a script on the
// operator's machine needs: a command that exits nonzero makes devbox exit nonzero.
func opExec(o ops) cli.Command {
	const usage = "devbox exec <name> -- <command>"
	return cli.Command{
		Name:    "exec",
		Summary: "Run one command on a box and print its combined output",
		Usage:   usage,
		Help:    "Exits nonzero when the remote command does, which is what a script on this machine needs.",
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			positional, err := cli.Parse(deps.FlagSet("exec"), args)
			if err != nil {
				return parseError(err, usage)
			}
			name, err := firstBox(positional, usage)
			if err != nil {
				return err
			}
			remote := strings.Join(positional[1:], " ")
			if remote == "" {
				return usageError(usage)
			}
			if dryRun(deps, sshArgv(deps.Config.SSHHost(name.String()), remote)...) {
				return nil
			}
			session, err := o.open(ctx, deps, name)
			if err != nil {
				return err
			}
			out, err := session.Run(ctx, remote)
			printOutput(deps, out)
			return err
		},
	}
}

// opCopy moves one path in either direction.
func opCopy(o ops) cli.Command {
	const usage = "devbox cp <name> <source> <target> [--down]"
	return cli.Command{
		Name:    "cp",
		Summary: "Copy a file or directory between this machine and a box",
		Usage:   usage,
		Help:    "--down copies from the box to this machine. It uses rsync, and never --delete: a copy may not remove anything on the other side.",
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			set := deps.FlagSet("cp")
			down := set.Bool("down", false, "copy from the box to this machine")
			paths, err := cli.Parse(set, args)
			if err != nil {
				return parseError(err, usage)
			}
			if len(paths) != 3 {
				return usageError(usage)
			}
			name, err := firstBox(paths, usage)
			if err != nil {
				return err
			}
			alias := deps.Config.SSHHost(name.String())
			if *down {
				if dryRun(deps, downloadArgv(alias, paths[1], paths[2])...) {
					return nil
				}
			} else if dryRun(deps, uploadArgv(alias, paths[1], paths[2])...) {
				return nil
			}
			session, err := o.open(ctx, deps, name)
			if err != nil {
				return err
			}
			if *down {
				if err := session.Download(ctx, paths[1], paths[2]); err != nil {
					return err
				}
				deps.Printf("downloaded %s:%s to %s", alias, paths[1], paths[2])
				return nil
			}
			if err := session.Upload(ctx, paths[1], paths[2]); err != nil {
				return err
			}
			deps.Printf("uploaded %s to %s:%s", paths[1], alias, paths[2])
			return nil
		},
	}
}

// opForward opens the port tunnel the operator's own tools use: a browser, a
// notebook server, or anything else that speaks to localhost but needs the box's
// network. The command is printed first so it can be run again without devbox.
func opForward() cli.Command {
	const usage = "devbox forward <name> [--port <port>]"
	return cli.Command{
		Name:    "forward",
		Summary: "Open an IAP tunnel from a local port to a box",
		Usage:   usage,
		Help:    "--port sets the local port, defaulting to port_forward in the configuration. The tunnel listens on localhost only.",
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			set := deps.FlagSet("forward")
			port := set.Int("port", 0, "the local port to forward; the configuration's port by default")
			positional, err := cli.Parse(set, args)
			if err != nil {
				return parseError(err, usage)
			}
			name, err := onlyBox(positional, usage)
			if err != nil {
				return err
			}
			if *port < 0 || *port > 65535 {
				return fmt.Errorf("invalid port %d\nusage: %s", *port, usage)
			}
			argv := forwardArgv(deps.Config, name, *port)
			deps.Printf("gcloud %s", strings.Join(argv, " "))
			if deps.DryRun {
				return nil
			}
			_, err = deps.Cloud.Run(ctx, argv...)
			return err
		},
	}
}

// forwardArgv opens a tunnel that listens on the operator's machine. The port is
// the configured one unless the operator names another, and the tunnel listens on
// localhost so nothing else on the network can reach it.
func forwardArgv(cfg config.Config, name box.Name, port int) []string {
	if port <= 0 {
		port = cfg.PortForward
	}
	if port <= 0 {
		port = defaultPortForward
	}
	listen := "localhost:" + strconv.Itoa(port)
	return []string{
		"compute", "start-iap-tunnel", name.String(), strconv.Itoa(port),
		"--local-host-port=" + listen, cfg.ProjectFlag(), cfg.ZoneFlag(),
	}
}

// defaultPortForward is the port devbox forwards when the configuration names
// none.
const defaultPortForward = 9224

// opTerminfo installs the Ghostty terminal entry on the box. The entry is read
// here and fed to the box's tic, because the local terminfo database and the
// box's have nothing to do with each other.
func opTerminfo(o ops) cli.Command {
	const usage = "devbox terminfo <name>"
	return cli.Command{
		Name:    "terminfo",
		Summary: "Install the Ghostty xterm-ghostty entry on a box",
		Usage:   usage,
		Help:    "Run it once per box. It reads the local entry with infocmp and prints the SetEnv fallback for programs that cannot see it.",
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			positional, err := cli.Parse(deps.FlagSet("terminfo"), args)
			if err != nil {
				return parseError(err, usage)
			}
			name, err := onlyBox(positional, usage)
			if err != nil {
				return err
			}
			entry, err := terminfoEntry(ctx, o)
			if err != nil {
				deps.Printf("%s", terminfoFallback)
				return err
			}
			alias, err := reach(ctx, o, deps, name)
			if err != nil {
				return err
			}
			if dryRun(deps, sshArgv(alias, "tic -x -")...) {
				deps.Printf("%s", terminfoFallback)
				return nil
			}
			if err := o.proc(ctx, sshArgv(alias, "tic -x -"), strings.NewReader(entry), deps.Out, deps.Err); err != nil {
				deps.Printf("%s", terminfoFallback)
				return fmt.Errorf("install the xterm-ghostty entry on %s: %w", alias, err)
			}
			deps.Printf("installed xterm-ghostty for %s", alias)
			deps.Printf("%s", terminfoFallback)
			return nil
		},
	}
}

// terminfoFallback is what the operator can put in the box's Host entry when a
// program cannot read the entry devbox installed.
const terminfoFallback = "SetEnv TERM=xterm-256color"

// infocmpCandidates are tried in order. The system infocmp on macOS is older than
// the Ghostty entry and reports the terminal as unknown, so the Homebrew ncurses
// build, which tracks current terminfo, is the second choice; that path simply
// does not exist elsewhere.
var infocmpCandidates = []string{"infocmp", "/opt/homebrew/opt/ncurses/bin/infocmp"}

// terminfoEntry renders the local xterm-ghostty entry in the source form tic
// reads. The extended form is required on both ends: without it the capabilities
// the Ghostty entry relies on are dropped.
func terminfoEntry(ctx context.Context, o ops) (string, error) {
	var problems []string
	for _, candidate := range infocmpCandidates {
		var out bytes.Buffer
		if err := o.proc(ctx, []string{candidate, "-x", "xterm-ghostty"}, nil, &out, &out); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", candidate, err))
			continue
		}
		if strings.TrimSpace(out.String()) == "" {
			problems = append(problems, candidate+": reported no entry")
			continue
		}
		return out.String(), nil
	}
	return "", fmt.Errorf("no infocmp could read the xterm-ghostty entry (%s)", strings.Join(problems, "; "))
}

// opEditors prints what the operator pastes into an editor. Both editors resolve
// the alias through the same SSH configuration, so the box needs no editor-side
// setup at all.
func opEditors() cli.Command {
	const usage = "devbox editors <name> [path]"
	return cli.Command{
		Name:    "editors",
		Summary: "Print the editor URLs that open a directory on a box",
		Usage:   usage,
		Help:    "Prints a zed ssh:// url and the VS Code Remote-SSH host. Both resolve through the SSH configuration devbox writes, so no editor setup is needed.",
		Run: func(_ context.Context, deps cli.Deps, args []string) error {
			positional, err := cli.Parse(deps.FlagSet("editors"), args)
			if err != nil {
				return parseError(err, usage)
			}
			if len(positional) < 1 || len(positional) > 2 {
				return usageError(usage)
			}
			name, err := firstBox(positional, usage)
			if err != nil {
				return err
			}
			home := remoteHome(deps.Config)
			path := home
			if len(positional) == 2 {
				path = positional[1]
				if !strings.HasPrefix(path, "/") {
					// A relative path is read from the home directory, which is
					// also where a path-less command lands.
					path = strings.TrimSuffix(home, "/") + "/" + path
				}
			}
			alias := deps.Config.SSHHost(name.String())
			deps.Printf("zed ssh://%s%s", alias, path)
			deps.Printf("VS Code Remote-SSH host: %s", alias)
			return nil
		},
	}
}

// remoteHome is the directory an editor opens when the operator names none. OS
// Login gives every user a home under /home, and root is the one exception.
func remoteHome(cfg config.Config) string {
	if cfg.RemoteUser == "root" {
		return "/root"
	}
	return "/home/" + cfg.RemoteUser
}

// firstBox parses the box name that opens a verb's positional arguments.
func firstBox(positional []string, usage string) (box.Name, error) {
	if len(positional) == 0 {
		return "", usageError(usage)
	}
	return box.ParseName(positional[0])
}

// onlyBox parses the single box name a verb takes and refuses anything else.
func onlyBox(positional []string, usage string) (box.Name, error) {
	if len(positional) != 1 {
		return "", usageError(usage)
	}
	return box.ParseName(positional[0])
}

// parseError adds the usage line to the argument parser's complaint, so the
// operator sees the form devbox expects and not only the flag it rejected.
func parseError(err error, usage string) error {
	return fmt.Errorf("%w\nusage: %s", err, usage)
}

// usageError reports a command line devbox cannot act on.
func usageError(usage string) error { return fmt.Errorf("usage: %s", usage) }

// printOutput writes remote output exactly as it came back, adding the newline a
// command without one would otherwise lose against the caller's next line.
func printOutput(deps cli.Deps, out string) {
	if out == "" {
		return
	}
	fmt.Fprint(deps.Out, out)
	if !strings.HasSuffix(out, "\n") {
		fmt.Fprintln(deps.Out)
	}
}
