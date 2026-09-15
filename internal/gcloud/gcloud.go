// Package gcloud runs the Google Cloud CLI.
//
// Every cloud mutation in devbox goes through an Executor, so an operation is
// always an explicit argument vector: visible under --dry-run, replayable from a
// record, and testable without a project. Nothing here builds a shell string.
package gcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Executor runs one gcloud invocation and returns its standard output.
type Executor interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// Runner is the real Executor. DryRun prints the exact command instead of
// running it, which is how devbox shows its blast radius before it spends money.
type Runner struct {
	Executable string
	DryRun     bool
	Output     io.Writer
}

// Run executes gcloud with separate arguments.
func (r Runner) Run(ctx context.Context, args ...string) (string, error) {
	binary := r.Executable
	if binary == "" {
		binary = "gcloud"
	}
	if r.DryRun {
		if r.Output != nil {
			fmt.Fprintf(r.Output, "%s %s\n", binary, strings.Join(args, " "))
		}
		return "", nil
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(string(out))
		}
		if detail == "" {
			return "", fmt.Errorf("%s %s: %w", binary, strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("%s %s: %s: %w", binary, strings.Join(args, " "), detail, err)
	}
	return string(out), nil
}

// JSON runs gcloud with --format=json and decodes the result into value.
func (r Runner) JSON(ctx context.Context, value any, args ...string) error {
	withFormat := append(append([]string{}, args...), "--format=json")
	out, err := r.Run(ctx, withFormat...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("gcloud returned no output for %s", strings.Join(args, " "))
	}
	if err := json.Unmarshal([]byte(out), value); err != nil {
		return fmt.Errorf("decode %s output: %w", strings.Join(args, " "), err)
	}
	return nil
}

// Missing reports whether an error means the named resource does not exist, which
// callers use to make provisioning idempotent without parsing human messages.
func Missing(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "was not found") ||
		strings.Contains(message, "could not be found") ||
		strings.Contains(message, "not found") ||
		strings.Contains(message, "does not exist")
}

// ErrDryRun is returned by callers that need to stop a workflow that would have
// written state, so a dry run never records a mutation as done.
var ErrDryRun = errors.New("dry run")
