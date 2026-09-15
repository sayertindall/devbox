// Package tools manages the toolchain a box installs.
//
// The pins live in the [tools] table of ~/.devbox/config.toml and are the only
// source of truth: a new box gets them from the startup script, and an existing
// box converges with `devbox tools apply`. Versions are resolved from mise (and
// from npm for the agent harness), never typed from memory.
package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Resolver answers what the newest published version of a package is.
type Resolver interface {
	Latest(ctx context.Context, name string) (string, error)
}

// Mise resolves a tool by asking the same tool manager the box uses.
type Mise struct {
	Executable string
}

// Latest runs mise latest <name> and returns the version it prints.
func (m Mise) Latest(ctx context.Context, name string) (string, error) {
	binary := m.Executable
	if binary == "" {
		binary = "mise"
	}
	out, err := exec.CommandContext(ctx, binary, "latest", name).Output()
	if err != nil {
		return "", fmt.Errorf("resolve newest %s with %s: %w", name, binary, err)
	}
	return parseVersion(string(out), name)
}

// Npm resolves an npm package version, which is how the agent harness is pinned.
type Npm struct {
	Executable string
}

// Latest runs npm view <name> version and returns the version it prints.
func (n Npm) Latest(ctx context.Context, name string) (string, error) {
	binary := n.Executable
	if binary == "" {
		binary = "npm"
	}
	out, err := exec.CommandContext(ctx, binary, "view", name, "version").Output()
	if err != nil {
		return "", fmt.Errorf("resolve newest %s with %s: %w", name, binary, err)
	}
	return parseVersion(string(out), name)
}

// parseVersion takes the last non-empty line, which is how both mise and npm
// print a version when a wrapper has emitted warnings first.
func parseVersion(out, name string) (string, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	version := strings.TrimSpace(lines[len(lines)-1])
	if version == "" {
		return "", fmt.Errorf("%s reported no version", name)
	}
	return version, nil
}

// Recording is a Resolver test double.
type Recording struct {
	Replies map[string]string
	Err     error
	Asked   []string
}

// Latest returns the scripted version for a name.
func (r *Recording) Latest(_ context.Context, name string) (string, error) {
	r.Asked = append(r.Asked, name)
	if r.Err != nil {
		return "", r.Err
	}
	version, ok := r.Replies[name]
	if !ok {
		return "", fmt.Errorf("no scripted version for %s", name)
	}
	return version, nil
}
