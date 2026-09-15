package access

import (
	"bytes"
	"context"
	"errors"
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
	open func(deps cli.Deps, name box.Name) (Session, error)
	proc proc
}

// realOps is the production wiring of ops.
func realOps() ops {
	return ops{
		open: func(deps cli.Deps, name box.Name) (Session, error) {
			return Dialer{Config: deps.Config, Out: deps.Out, Err: deps.Err, Stdin: deps.Stdin}.Open(name)
		},
		proc: runProcess,
	}
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
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			name, err := boxName(args, usage)
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
			block, err := writeSSHBlock(path, deps.Config, name, facts.ExternalIP())
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
			if facts.ExternalIP() == "" {
				deps.Printf("connections to %s run through an IAP tunnel", name)
			}
			return nil
		},
	}
}

// describeArgs reads one instance back. It is the only way devbox learns which of
// the two SSH entries a box needs, because a box with an external address is
// reached directly and a box without one needs the tunnel.
func describeArgs(cfg config.Config, name box.Name) []string {
	return []string{
		"compute", "instances", "describe", name.String(),
		cfg.ZoneFlag(), cfg.ProjectFlag(), "--format=json",
	}
}

// opSSH opens the operator's own connection: a shell, or one command with the
// terminal streaming through so a long command can be watched and interrupted.
func opSSH(o ops) cli.Command {
	const usage = "devbox ssh <name> [-- command]"
	return cli.Command{
		Name:    "ssh",
		Summary: "Open a shell on a box, or run one command through it",
		Usage:   usage,
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			name, err := boxName(args, usage)
			if err != nil {
				return err
			}
			argv := interactiveArgv(deps.Config.SSHHost(name.String()), command(args[1:]))
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
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			name, err := boxName(args, usage)
			if err != nil {
				return err
			}
			remote := command(args[1:])
			if remote == "" {
				return usageError(usage)
			}
			session, err := o.open(deps, name)
			if err != nil {
				return err
			}
			out, err := session.Run(ctx, remote)
			printOutput(deps, out)
			return err
		},
	}
}

// opCopy moves one path in either direction. The direction flag may be written
// before or after the paths, because an operator fixing up a command tends to
// append it.
func opCopy(o ops) cli.Command {
	const usage = "devbox cp <name> <source> <target> [--down]"
	return cli.Command{
		Name:    "cp",
		Summary: "Copy a file or directory between this machine and a box",
		Usage:   usage,
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			paths, down, err := splitDirection(args)
			if err != nil {
				return err
			}
			if len(paths) != 3 {
				return usageError(usage)
			}
			name, err := boxName(paths, usage)
			if err != nil {
				return err
			}
			session, err := o.open(deps, name)
			if err != nil {
				return err
			}
			alias := deps.Config.SSHHost(name.String())
			if down {
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
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			rest, port, err := splitPort(args)
			if err != nil {
				return err
			}
			name, err := boxName(rest, usage)
			if err != nil {
				return err
			}
			argv := forwardArgv(deps.Config, name, port)
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
// the configured one unless the operator names another, and the tunnel is opened
// on localhost so nothing on the network can reach it.
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
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			name, err := boxName(args, usage)
			if err != nil {
				return err
			}
			entry, err := terminfoEntry(ctx, o)
			if err != nil {
				deps.Printf("%s", terminfoFallback)
				return err
			}
			alias := deps.Config.SSHHost(name.String())
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
// build, which tracks current terminfo, is the second choice; the path simply does
// not exist elsewhere.
var infocmpCandidates = []string{"infocmp", "/opt/homebrew/opt/ncurses/bin/infocmp"}

// terminfoEntry renders the local xterm-ghostty entry in the source form tic
// reads. --x is required on both ends: without it the extended capabilities the
// Ghostty entry relies on are dropped.
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
		Run: func(_ context.Context, deps cli.Deps, args []string) error {
			if len(args) == 0 || len(args) > 2 {
				return usageError(usage)
			}
			name, err := boxName(args, usage)
			if err != nil {
				return err
			}
			path := remoteHome(deps.Config)
			if len(args) == 2 {
				path = args[1]
				if !strings.HasPrefix(path, "/") {
					// A relative path is read from the home directory, which is
					// also where a path-less command lands.
					path = strings.TrimSuffix(remoteHome(deps.Config), "/") + "/" + path
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

// boxName parses the box argument that opens every access verb.
func boxName(args []string, usage string) (box.Name, error) {
	if len(args) == 0 {
		return "", usageError(usage)
	}
	return box.ParseName(args[0])
}

// command joins the operator's own words after a box name into one remote
// command. It is one string because the box's shell, not this one, decides how
// the words are split, so an operator who quotes a command keeps it exact.
func command(args []string) string {
	rest := args
	for len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	return strings.Join(rest, " ")
}

// splitDirection pulls the copy direction out of the argument list.
func splitDirection(args []string) ([]string, bool, error) {
	var paths []string
	down := false
	for _, arg := range args {
		switch {
		case arg == "--down":
			down = true
		case arg == "--":
		case strings.HasPrefix(arg, "-"):
			return nil, false, fmt.Errorf("unknown flag %q\nusage: devbox cp <name> <source> <target> [--down]", arg)
		default:
			paths = append(paths, arg)
		}
	}
	return paths, down, nil
}

// splitPort pulls the forwarded port out of the argument list.
func splitPort(args []string) ([]string, int, error) {
	var rest []string
	port := 0
	for index := 0; index < len(args); index++ {
		arg := args[index]
		value := ""
		switch {
		case arg == "--":
			continue
		case arg == "--port":
			if index+1 >= len(args) {
				return nil, 0, errors.New("--port needs a port number\nusage: devbox forward <name> [--port <port>]")
			}
			index++
			value = args[index]
		case strings.HasPrefix(arg, "--port="):
			value = strings.TrimPrefix(arg, "--port=")
		case strings.HasPrefix(arg, "-"):
			return nil, 0, fmt.Errorf("unknown flag %q\nusage: devbox forward <name> [--port <port>]", arg)
		default:
			rest = append(rest, arg)
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 || parsed > 65535 {
			return nil, 0, fmt.Errorf("invalid port %q", value)
		}
		port = parsed
	}
	return rest, port, nil
}

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

// usageError reports a command line devbox cannot act on.
func usageError(usage string) error { return fmt.Errorf("usage: %s", usage) }
