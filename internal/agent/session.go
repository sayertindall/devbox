// Package agent runs agent sessions on a box so a long turn survives the laptop
// closing: it starts a detached tmux session from a handoff packet, lists what is
// running, reads a pane, attaches to it, and stops it.
//
// A session is a tmux session named devbox-<id> on the box. Every start is
// written to the durable record store before the first remote call, so a dropped
// connection leaves a session the operator can still name, inspect, and reconcile
// instead of a process nobody can account for.
package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
)

// Provider is an agent harness devbox can start on a box. The name is also the
// program the detached session runs: the bootstrap installs each harness on the
// PATH such a session inherits.
type Provider string

const (
	ProviderOmp    Provider = "omp"
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

// providers is the closed set devbox will start, so a mistyped provider cannot
// run an arbitrary program on the box.
var providers = []Provider{ProviderOmp, ProviderClaude, ProviderCodex}

// ParseProvider accepts one of the harnesses devbox installs.
func ParseProvider(value string) (Provider, error) {
	for _, provider := range providers {
		if string(provider) == value {
			return provider, nil
		}
	}
	return "", fmt.Errorf("unsupported provider %q: use %s", value, providerList())
}

// providerList names the providers in the order the usage line reads.
func providerList() string { return "omp, claude, or codex" }

// sessionPrefix marks every tmux session devbox starts. Nothing devbox does
// touches a session without it, so another tool's session is never listed as a
// session devbox could stop.
const sessionPrefix = "devbox-"

// sessionIDBytes is the size of a session id. The id is hex, which tmux accepts
// in a target and a shell needs no quoting for.
const sessionIDBytes = 8

// handoffFile and statusFile are the two files inside a session directory: the
// packet the session works from, and the marker the session writes when it ends.
const (
	handoffFile = "handoff.json"
	statusFile  = "status"
)

// newSessionID returns a fresh session id.
func newSessionID() (string, error) {
	var raw [sessionIDBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// ParseSessionRef accepts the id of a session. tmux reads a colon, a period, and
// a comma as target separators, so an id holding one could address a different
// session than the operator named.
func ParseSessionRef(value string) (string, error) {
	if value == "" || len(value) > 64 {
		return "", fmt.Errorf("invalid session id %q", value)
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_':
		default:
			return "", fmt.Errorf("invalid session id %q: letters, digits, hyphen, and underscore only", value)
		}
	}
	return value, nil
}

// sessionName is the tmux session name for one id.
func sessionName(ref string) string { return sessionPrefix + ref }

// remoteSessionDir is a session's directory on the box, relative to the remote
// home so scp and rsync resolve it without a shell.
func remoteSessionDir(ref string) string { return box.AgentRoot + "/" + ref }

// remoteHandoff is the packet path on the box.
func remoteHandoff(ref string) string { return remoteSessionDir(ref) + "/" + handoffFile }

// remoteStatus is the path the session writes when the harness exits, which is
// how devbox reports that a run finished.
func remoteStatus(ref string) string { return remoteSessionDir(ref) + "/" + statusFile }

// localHandoff is the operator's copy of the packet. It is written before the
// upload so the operator can read exactly what the box was handed.
func localHandoff(boxName, ref string) (string, error) {
	dir, err := config.StatePath("agents", boxName, ref)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, handoffFile), nil
}

// shellQuote returns value as one single-quoted shell word that expands to
// exactly value.
//
// The string devbox sends to the box crosses two shells: the login shell ssh
// starts and the shell tmux runs the session command with. A task holding a
// quote, a dollar sign, or a backtick must reach the harness unchanged, so every
// literal goes through this function, which closes the quote, escapes the single
// quote, and reopens it.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// pathExpr renders a devbox-owned path under the remote home as a shell word for
// the box: the home is expanded, and the rest is literal. The path is built from
// a validated name and a hex session id, so it holds nothing a shell would expand
// inside double quotes.
func pathExpr(relative string) string { return `"$HOME/` + relative + `"` }

// remoteDirExpr renders the working directory as a shell word for the box.
//
// The path has to stay literal inside double quotes, so a directory holding a
// quote, a dollar sign, a backtick, or a backslash is refused here rather than
// silently mangled into a different directory on the box.
func remoteDirExpr(dir string) (string, error) {
	value := strings.TrimSpace(dir)
	if value == "" {
		return "", errors.New("working directory is empty")
	}
	if !strings.HasPrefix(value, "/") && value != "~" && !strings.HasPrefix(value, "~/") {
		return "", fmt.Errorf("working directory %q must be absolute or start with ~", dir)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return "", fmt.Errorf("working directory %q holds a control character", dir)
		}
		if char == '"' || char == '\'' || char == '$' || char == '`' || char == '\\' {
			return "", fmt.Errorf("working directory %q holds a character devbox cannot quote safely", dir)
		}
	}
	switch {
	case value == "~":
		value = "$HOME"
	case strings.HasPrefix(value, "~/"):
		value = "$HOME/" + value[2:]
	}
	return `"` + value + `"`, nil
}

// mkdirCommand creates the session directory. It runs first because the upload
// needs its destination to exist.
func mkdirCommand(ref string) string {
	return "mkdir -p " + pathExpr(remoteSessionDir(ref))
}

// launchCommand starts the session detached, from the packet devbox uploaded.
//
// tmux runs the command with the box's shell, so the working directory, the
// packet path, and the status path are quoted for that shell and the whole
// command is quoted again for the login shell ssh starts. The session outlives
// the client because tmux owns the process, not the ssh connection: closing the
// laptop ends the connection and nothing else.
func launchCommand(request request, ref string) string {
	inner := "cd " + request.RemoteDir +
		" && " + string(request.Provider) +
		" --task-file " + pathExpr(remoteHandoff(ref)) +
		" " + shellQuote(request.Task) +
		"; echo done > " + pathExpr(remoteStatus(ref))
	return "tmux new-session -d -s " + sessionName(ref) + " " + shellQuote(inner)
}

// listCommand asks tmux for the session name and its creation time. The creation
// time is a Unix epoch, so devbox never has to guess the box's timezone to work
// out a session's age.
func listCommand() string { return "tmux ls -F '#{session_name} #{session_created}'" }

// captureCommand reads the visible pane and reaches 200 lines back, so a turn
// that has already scrolled off the screen is still readable.
func captureCommand(ref string) string {
	return "tmux capture-pane -p -t " + sessionName(ref) + " -S -200"
}

// killCommand ends the session and everything running under it.
func killCommand(ref string) string { return "tmux kill-session -t " + sessionName(ref) }

// statusCommand reports what a finished session wrote. A session that is still
// running has written nothing, and a missing file is not an error.
func statusCommand(ref string) string {
	path := pathExpr(remoteStatus(ref))
	return "if [ -f " + path + " ]; then cat " + path + "; fi"
}

// request is one validated session start: what the operator asked for, reduced to
// the facts the packet, the record, and the remote command are built from.
type request struct {
	Box          box.Name
	Provider     Provider
	Task         string
	Tree         string
	Dir          string // as the operator wrote it, or the default devbox chose
	RemoteDir    string // shell word for the working directory on the box
	CurrentState string
	NextAction   string
	Constraints  []string
}

// newRequest validates a start and derives the handoff text.
//
// The state, the next action, and the constraints are derived rather than asked
// for because a session that starts with an empty handoff is worse than one that
// starts with the facts devbox already knows: where the tree came from, what the
// box costs, and which of its parts may not move.
func newRequest(name box.Name, providerValue, task, tree, dir string) (request, error) {
	provider, err := ParseProvider(providerValue)
	if err != nil {
		return request{}, err
	}
	task = strings.TrimSpace(task)
	if task == "" {
		return request{}, errors.New("a task is required: pass --task")
	}
	if tree != "" {
		// A tree name becomes a path component under the tree root, so it follows
		// the same grammar as a box name.
		if _, err := box.ParseName(tree); err != nil {
			return request{}, fmt.Errorf("tree name %q: %w", tree, err)
		}
	}
	if strings.TrimSpace(dir) == "" {
		dir = "~"
		if tree != "" {
			dir = "~/" + box.TreeRoot + "/" + tree
		}
	}
	remoteDir, err := remoteDirExpr(dir)
	if err != nil {
		return request{}, err
	}
	dir = strings.TrimSpace(dir)
	return request{
		Box:          name,
		Provider:     provider,
		Task:         task,
		Tree:         tree,
		Dir:          dir,
		RemoteDir:    remoteDir,
		CurrentState: currentState(name, tree, dir),
		NextAction:   fmt.Sprintf("run the %s session on the task above", provider),
		Constraints:  constraints(dir),
	}, nil
}

// currentState describes what the session starts from, in the words the packet
// needs: which tree, and where on the box the session will find it.
func currentState(name box.Name, tree, dir string) string {
	if tree == "" {
		return fmt.Sprintf("box %s is running with no tree pushed; the session starts in %s", name, dir)
	}
	return fmt.Sprintf("tree %q is pushed to box %s; the session starts in %s", tree, name, dir)
}

// constraints are the rules a session inherits from the box's shape. They are the
// decisions a reader inside the session cannot see: the caches live on the data
// disk because local SSD is discarded on stop, and a snapshot is a
// crash-consistent disk image, so the engines have to be stopped first.
func constraints(dir string) []string {
	return []string{
		"work inside " + dir + ": the tree devbox pushed is the tree devbox reads back",
		"leave the box running when the work is done; devbox stops the machine, the session does not",
		"keep build state on the data disk: " + box.DockerRoot + ", " + box.ContainerdRoot + ", " + box.DaggerCache + ", and " + box.PNPMStore,
		"stop Docker and containerd before devbox snapshots the box, because a disk snapshot is crash-consistent",
	}
}

// dialer builds the session factory for one box from the command's dependencies.
func dialer(deps cli.Deps) access.Dialer {
	return access.Dialer{Config: deps.Config, Out: deps.Out, Err: deps.Err, Stdin: deps.Stdin}
}
