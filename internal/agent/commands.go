package agent

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/packet"
	"devbox/internal/record"
)

// usage is the one place the agent grammar is written down.
const usage = `usage:
  devbox agent start <box> --provider <omp|claude|codex> --task <text> [--tree <name>] [--dir <dir>]
  devbox agent list <box>
  devbox agent logs <box> <id>
  devbox agent attach <box> <id>
  devbox agent stop <box> <id> [--yes]
  devbox agent reconcile <box> <id> [--detail <text>] [--yes]`

// Commands returns the agent verb.
func Commands() []cli.Command {
	return []cli.Command{{
		Name:    "agent",
		Summary: "Start, list, watch, attach to, and stop agent sessions on a box",
		Usage:   "devbox agent <start|list|logs|attach|stop|reconcile> ...",
		Run:     dispatch,
	}}
}

// dispatch routes one agent subcommand. Every subcommand names the box it acts
// on, so a session is never addressed without saying where it runs.
func dispatch(ctx context.Context, deps cli.Deps, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "start":
		return start(ctx, deps, args[1:])
	case "list":
		return list(ctx, deps, args[1:])
	case "logs":
		return logs(ctx, deps, args[1:])
	case "attach":
		return attach(ctx, deps, args[1:], runExternal)
	case "stop":
		return stop(ctx, deps, args[1:])
	case "reconcile":
		return reconcile(ctx, deps, args[1:])
	default:
		return fmt.Errorf("unknown agent subcommand %q\n\n%s", args[0], usage)
	}
}

// positional returns the arguments left after flag parsing, refusing anything
// other than the exact count the subcommand takes.
func positional(set *flag.FlagSet, count int) ([]string, error) {
	rest := set.Args()
	if len(rest) != count {
		return nil, errors.New(usage)
	}
	return rest, nil
}

// start validates a start, refuses it if an earlier session is unresolved, and
// hands it to the box.
func start(ctx context.Context, deps cli.Deps, args []string) error {
	set := deps.FlagSet("agent start")
	provider := set.String("provider", "", "harness to run: "+providerList())
	task := set.String("task", "", "task the session works on")
	tree := set.String("tree", "", "tree already pushed to the box to work in")
	dir := set.String("dir", "", "working directory on the box (default: the tree, otherwise the remote home)")
	if err := set.Parse(args); err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	rest, err := positional(set, 1)
	if err != nil {
		return err
	}
	name, err := box.ParseName(rest[0])
	if err != nil {
		return err
	}
	request, err := newRequest(name, *provider, *task, *tree, *dir)
	if err != nil {
		return err
	}
	if err := refusal(deps.Records, request); err != nil {
		return err
	}
	ref, err := newSessionID()
	if err != nil {
		return err
	}
	if deps.DryRun {
		return printStart(deps, request, ref)
	}
	client, err := dialer(deps).Open(name)
	if err != nil {
		return err
	}
	return startSession(ctx, deps, client, request, ref)
}

// printStart reports the exact steps a start would take. A dry run writes no
// packet, uploads nothing, and records nothing, because a dry run that left a
// record behind would block the real start it was rehearsing.
func printStart(deps cli.Deps, request request, ref string) error {
	local, err := localHandoff(string(request.Box), ref)
	if err != nil {
		return err
	}
	deps.Printf("dry run: write %s", local)
	deps.Printf("dry run: %s", mkdirCommand(ref))
	deps.Printf("dry run: upload %s to %s:%s", local, deps.Config.SSHHost(string(request.Box)), remoteSessionDir(ref))
	deps.Printf("dry run: %s", launchCommand(request, ref))
	return nil
}

// startSession writes the packet, uploads it, and starts the detached session.
//
// The order is the point: the record is written before the first remote call, and
// the packet is on the box before the session that reads it exists. A session that
// started first would work from a task it never received.
func startSession(ctx context.Context, deps cli.Deps, client access.Session, request request, ref string) error {
	if deps.Records == nil {
		return errors.New("agent start needs a record store")
	}
	local, err := localHandoff(string(request.Box), ref)
	if err != nil {
		return err
	}
	handoff, err := buildHandoff(request)
	if err != nil {
		return err
	}
	encoded, err := packet.Encode(handoff)
	if err != nil {
		return err
	}
	if err := writeHandoff(local, encoded); err != nil {
		return err
	}
	launch := launchCommand(request, ref)
	entry, err := deps.Records.Begin(KindSession, string(request.Box), sessionArgs(request, ref, launch))
	if err != nil {
		return err
	}
	if _, err := client.Run(ctx, mkdirCommand(ref)); err != nil {
		// The session directory does not exist, so the session cannot exist
		// either: this start provably did nothing and must not block the next one.
		recordFailed(deps, entry, err.Error())
		return fmt.Errorf("prepare session %s on %s: %w", sessionName(ref), request.Box, err)
	}
	if err := client.Upload(ctx, local, remoteSessionDir(ref)); err != nil {
		recordFailed(deps, entry, err.Error())
		return fmt.Errorf("upload handoff for session %s on %s: %w", sessionName(ref), request.Box, err)
	}
	if _, err := client.Run(ctx, launch); err != nil {
		// tmux may have created the session before the connection dropped, so the
		// outcome is unresolved until an operator reconciles it.
		recordUnknown(deps, entry, err.Error())
		return fmt.Errorf("start session %s on %s: %w", sessionName(ref), request.Box, err)
	}
	if err := deps.Records.Known(entry, sessionName(ref)); err != nil {
		return err
	}
	deps.Printf("started %s on %s (%s%s)", sessionName(ref), request.Box, request.Provider, treeSuffix(request.Tree))
	deps.Printf("handoff %s", local)
	deps.Printf("logs    devbox agent logs %s %s", request.Box, ref)
	deps.Printf("attach  devbox agent attach %s %s", request.Box, ref)
	return nil
}

// list merges the durable notes with what tmux reports for one box.
func list(ctx context.Context, deps cli.Deps, args []string) error {
	set := deps.FlagSet("agent list")
	if err := set.Parse(args); err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	rest, err := positional(set, 1)
	if err != nil {
		return err
	}
	name, err := box.ParseName(rest[0])
	if err != nil {
		return err
	}
	records, err := sessionRecords(deps.Records, name)
	if err != nil {
		return err
	}
	var live []tmuxSession
	if deps.DryRun {
		deps.Printf("dry run: %s", listCommand())
	} else {
		client, err := dialer(deps).Open(name)
		if err != nil {
			return err
		}
		live = liveSessions(ctx, deps, client, name)
	}
	return render(deps.Out, name, records, live)
}

// logs reads the session's pane, which is where a harness writes when nobody is
// watching it.
func logs(ctx context.Context, deps cli.Deps, args []string) error {
	set := deps.FlagSet("agent logs")
	if err := set.Parse(args); err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	rest, err := positional(set, 2)
	if err != nil {
		return err
	}
	name, err := box.ParseName(rest[0])
	if err != nil {
		return err
	}
	ref, err := ParseSessionRef(rest[1])
	if err != nil {
		return err
	}
	command := captureCommand(ref)
	if deps.DryRun {
		deps.Printf("dry run: %s", command)
		return nil
	}
	client, err := dialer(deps).Open(name)
	if err != nil {
		return err
	}
	output, err := client.Run(ctx, command)
	if err != nil {
		return fmt.Errorf("read session %s on %s: %w", sessionName(ref), name, err)
	}
	text := strings.TrimRight(output, "\n")
	if strings.TrimSpace(text) == "" {
		deps.Printf("session %s has drawn nothing yet", sessionName(ref))
		return nil
	}
	deps.Printf("%s", text)
	return nil
}

// attach hands the operator's terminal to the session. The session keeps running
// whether or not a client is attached, so detaching is safe.
func attach(ctx context.Context, deps cli.Deps, args []string, run external) error {
	set := deps.FlagSet("agent attach")
	if err := set.Parse(args); err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	rest, err := positional(set, 2)
	if err != nil {
		return err
	}
	name, err := box.ParseName(rest[0])
	if err != nil {
		return err
	}
	ref, err := ParseSessionRef(rest[1])
	if err != nil {
		return err
	}
	argv := attachArgv(deps.Config, name, ref)
	if deps.DryRun {
		deps.Printf("dry run: %s", strings.Join(argv, " "))
		return nil
	}
	return run(ctx, argv, deps.Stdin, deps.Out, deps.Err)
}

// stop ends a session. It is destructive, so it says what it will kill and waits
// for an explicit yes before the box hears anything.
func stop(ctx context.Context, deps cli.Deps, args []string) error {
	set := deps.FlagSet("agent stop")
	yes := set.Bool("yes", false, "kill the session without asking")
	if err := set.Parse(args); err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	rest, err := positional(set, 2)
	if err != nil {
		return err
	}
	name, err := box.ParseName(rest[0])
	if err != nil {
		return err
	}
	ref, err := ParseSessionRef(rest[1])
	if err != nil {
		return err
	}
	session, err := findSession(deps.Records, name, ref)
	if err != nil {
		return err
	}
	deps.Printf("session %s on %s: provider %s, tree %s, recorded %s, %s old",
		sessionName(ref), name, display(string(session.Provider)), display(session.Tree), session.Entry.State, age(session.Entry.CreatedAt, time.Now().UTC()))
	deps.Printf("killing it ends the agent's turn and discards the pane scrollback")
	if deps.DryRun {
		deps.Printf("dry run: %s", killCommand(ref))
		return nil
	}
	if !*yes {
		if err := confirm(deps, fmt.Sprintf("kill %s on %s? type yes", sessionName(ref), name)); err != nil {
			return err
		}
	}
	client, err := dialer(deps).Open(name)
	if err != nil {
		return err
	}
	return stopSession(ctx, deps, client, session)
}

// stopSession kills the session and records the outcome. The record reaches known
// only after tmux reported the kill: a failed kill leaves the session unresolved,
// because devbox cannot tell a dropped connection from a session that still runs.
func stopSession(ctx context.Context, deps cli.Deps, client access.Session, session sessionRecord) error {
	if _, err := client.Run(ctx, killCommand(session.Ref)); err != nil {
		recordUnknown(deps, session.Entry, err.Error())
		return fmt.Errorf("kill session %s: %w", sessionName(session.Ref), err)
	}
	if err := deps.Records.Known(session.Entry, sessionName(session.Ref)); err != nil {
		return err
	}
	deps.Printf("killed %s", sessionName(session.Ref))
	output, err := client.Run(ctx, statusCommand(session.Ref))
	if err != nil {
		deps.Errorf("read %s: %v", remoteStatus(session.Ref), err)
		return nil
	}
	if text := strings.TrimSpace(output); text != "" {
		deps.Printf("status %s", text)
	}
	return nil
}

// reconcile clears a session whose outcome devbox never established, using what
// tmux reports as the evidence. It is the only way past a refusal, so it always
// shows the evidence and what it is about to record.
func reconcile(ctx context.Context, deps cli.Deps, args []string) error {
	set := deps.FlagSet("agent reconcile")
	detail := set.String("detail", "", "note to store with the resolution")
	yes := set.Bool("yes", false, "resolve without asking")
	if err := set.Parse(args); err != nil {
		return fmt.Errorf("%w\n\n%s", err, usage)
	}
	rest, err := positional(set, 2)
	if err != nil {
		return err
	}
	name, err := box.ParseName(rest[0])
	if err != nil {
		return err
	}
	ref, err := ParseSessionRef(rest[1])
	if err != nil {
		return err
	}
	session, err := findSession(deps.Records, name, ref)
	if err != nil {
		return err
	}
	if !session.unresolved() {
		deps.Printf("session %s on %s is already %s; nothing to reconcile", sessionName(ref), name, session.Entry.State)
		return nil
	}
	if deps.DryRun {
		deps.Printf("dry run: %s", listCommand())
		deps.Printf("dry run: resolve record %s from what tmux reports", session.Entry.ID)
		return nil
	}
	client, err := dialer(deps).Open(name)
	if err != nil {
		return err
	}
	output, err := client.Run(ctx, listCommand())
	if err != nil {
		return fmt.Errorf("reconcile session %s on %s: tmux ls: %w", sessionName(ref), name, err)
	}
	live := liveRef(parseSessions(output), ref)
	note := strings.TrimSpace(*detail)
	if live {
		deps.Printf("tmux reports %s running on %s", sessionName(ref), name)
	} else {
		deps.Printf("tmux reports no session %s on %s", sessionName(ref), name)
	}
	entry := session.Entry
	entry.Result = ""
	action := "failed"
	resolution := "operator reconciliation: tmux reports no " + sessionName(ref)
	if live {
		entry.Result = sessionName(ref)
		action = "known"
		resolution = "operator reconciliation: tmux reports " + sessionName(ref) + " running"
	}
	if note != "" {
		resolution += "; " + note
	}
	deps.Printf("resolving record %s as %s", entry.ID, action)
	if !*yes {
		if err := confirm(deps, fmt.Sprintf("resolve session %s as %s? type yes", ref, action)); err != nil {
			return err
		}
	}
	if live {
		return deps.Records.Resolved(entry, resolution)
	}
	return deps.Records.Failed(entry, resolution)
}

// recordUnknown marks a start whose outcome devbox could not establish. The
// session may exist, so the note blocks the next start for the same provider until
// an operator reconciles it.
func recordUnknown(deps cli.Deps, entry record.Record, detail string) {
	if err := deps.Records.Unknown(entry, detail); err != nil {
		deps.Errorf("record session %s: %v", entry.ID, err)
	}
}

// recordFailed marks a start that provably produced no session, so it does not
// block the next one. A note that cannot be written is reported here because
// nothing later can recover it.
func recordFailed(deps cli.Deps, entry record.Record, detail string) {
	if err := deps.Records.Failed(entry, detail); err != nil {
		deps.Errorf("record session %s: %v", entry.ID, err)
	}
}

// treeSuffix names the tree in a start summary, and says nothing when the session
// works outside one.
func treeSuffix(tree string) string {
	if tree == "" {
		return ""
	}
	return ", tree " + tree
}

// confirm requires an explicit yes before devbox changes something the operator
// cannot undo from this terminal.
func confirm(deps cli.Deps, question string) error {
	if deps.Stdin == nil {
		return fmt.Errorf("%s: no terminal to ask on; pass --yes", question)
	}
	deps.Printf("%s", question)
	line, readErr := bufio.NewReader(deps.Stdin).ReadString('\n')
	if strings.EqualFold(strings.TrimSpace(line), "yes") {
		return nil
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return fmt.Errorf("read confirmation: %w", readErr)
	}
	return errors.New("cancelled: nothing was changed")
}
