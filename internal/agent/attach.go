package agent

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"devbox/internal/box"
	"devbox/internal/config"
)

// attachArgv is the interactive ssh devbox runs to hand the operator's terminal
// to the session: ssh asks the box for a tty, and tmux draws on it.
func attachArgv(cfg config.Config, boxName box.Name, ref string) []string {
	return []string{"ssh", "-t", cfg.SSHHost(string(boxName)), "tmux attach -t " + sessionName(ref)}
}

// external runs a program that takes over the operator's terminal.
type external func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error

// runExternal runs the interactive ssh and passes the operator's streams straight
// through, so keys, output, and the terminal size reach the session unmodified.
func runExternal(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return nil
}
