package gcloud

import (
	"context"
	"strings"
	"sync"
)

// Fake is a test double for Executor. Package tests across devbox use it to
// assert the exact argument vector a command builds and to script responses
// without a project.
type Fake struct {
	mu    sync.Mutex
	calls [][]string
	// Reply, when set, returns the output for one call. It receives the argument
	// vector so a test can answer describe calls differently from create calls.
	Reply func(args []string) (string, error)
}

// Calls returns a copy of every argument vector the Fake has seen.
func (f *Fake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// Last returns the most recent argument vector, or nil when nothing ran.
func (f *Fake) Last() []string {
	calls := f.Calls()
	if len(calls) == 0 {
		return nil
	}
	return calls[len(calls)-1]
}

// Argv joins every call into shell-like lines for a readable failure message.
func (f *Fake) Argv() string {
	var lines []string
	for _, call := range f.Calls() {
		lines = append(lines, strings.Join(call, " "))
	}
	return strings.Join(lines, "\n")
}

// Ran reports whether any recorded call contains every given substring.
func (f *Fake) Ran(fragments ...string) bool {
	for _, call := range f.Calls() {
		line := strings.Join(call, " ")
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(line, fragment) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// Run records the call and delegates to Reply.
func (f *Fake) Run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{}, args...))
	reply := f.Reply
	f.mu.Unlock()
	if reply == nil {
		return "", nil
	}
	return reply(args)
}
