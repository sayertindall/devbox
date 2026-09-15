package tools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
)

// session records the convergence command the apply verb sends to a box.
type session struct {
	commands []string
	output   string
	err      error
}

func (s *session) Run(_ context.Context, command string) (string, error) {
	s.commands = append(s.commands, command)
	return s.output, s.err
}

type dialer struct {
	sessions map[string]*session
}

func (d dialer) Open(_ context.Context, name box.Name) (Session, error) {
	if s, ok := d.sessions[name.String()]; ok {
		return s, nil
	}
	return nil, os.ErrNotExist
}

// harness writes a configuration file and returns the dependencies a command
// needs, so every test exercises the real file editing.
func harness(t *testing.T, tools string, ompVersion string) (cli.Deps, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	var out bytes.Buffer
	body := "project = \"example-project\"\nservice_account = \"devbox@example-project.iam.gserviceaccount.com\"\n"
	if ompVersion != "" {
		body += "omp_package = \"@oh-my-pi/pi-coding-agent\"\nomp_version = \"" + ompVersion + "\"\n"
	}
	if tools != "" {
		body += "\n[tools]\n" + tools
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cli.Deps{Config: cfg, ConfigPath: path, Out: &out, Err: &out}, path
}

func runCommand(t *testing.T, deps cli.Deps, mise, npm Resolver, d Dialer, args ...string) string {
	t.Helper()
	registry := cli.NewRegistry()
	registry.Add(CommandsWith(mise, npm, d)...)
	if err := registry.Run(context.Background(), deps, args); err != nil {
		t.Fatalf("devbox %s: %v", strings.Join(args, " "), err)
	}
	return deps.Out.(*bytes.Buffer).String()
}

func TestListShowsBuiltinsWhenNothingIsPinned(t *testing.T) {
	deps, _ := harness(t, "", "18.1.22")
	out := runCommand(t, deps, &Recording{}, &Recording{}, nil, "tools", "list")
	if !strings.Contains(out, "no [tools] pins declared") {
		t.Fatalf("list must say the built-in toolchain is in use:\n%s", out)
	}
	if !strings.Contains(out, "agent harness") {
		t.Fatalf("list must show the harness:\n%s", out)
	}
}

func TestAddMaterializesBuiltinsBeforePinning(t *testing.T) {
	deps, path := harness(t, "", "18.1.22")
	mise := &Recording{Replies: map[string]string{"ripgrep": "14.1.1"}}
	out := runCommand(t, deps, mise, &Recording{}, nil, "tools", "add", "ripgrep")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tools["ripgrep"] != "14.1.1" {
		t.Fatalf("new pin missing: %+v", cfg.Tools)
	}
	if len(cfg.Tools) < 2 {
		t.Fatalf("the built-in pins must be written too, otherwise the next box loses them: %+v", cfg.Tools)
	}
	if !strings.Contains(out, "built-in pins") {
		t.Fatalf("add must say what it did to the file:\n%s", out)
	}
	if len(mise.Asked) != 1 || mise.Asked[0] != "ripgrep" {
		t.Fatalf("add must resolve the newest version: %+v", mise.Asked)
	}
}

func TestAddAcceptsAnExplicitVersionWithoutResolving(t *testing.T) {
	deps, path := harness(t, "node = \"24.18.0\"\n", "18.1.22")
	mise := &Recording{Replies: map[string]string{}}
	runCommand(t, deps, mise, &Recording{}, nil, "tools", "add", "ripgrep@14.0.0")
	if len(mise.Asked) != 0 {
		t.Fatalf("an explicit version must not be resolved: %+v", mise.Asked)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tools["ripgrep"] != "14.0.0" {
		t.Fatalf("explicit pin missing: %+v", cfg.Tools)
	}
}

func TestUpdateRefreshesPinsAndHarness(t *testing.T) {
	deps, path := harness(t, "node = \"24.0.0\"\npython = \"3.12.14\"\n", "1.0.0")
	mise := &Recording{Replies: map[string]string{"node": "24.18.0", "python": "3.12.14"}}
	npm := &Recording{Replies: map[string]string{"@oh-my-pi/pi-coding-agent": "18.1.22"}}
	out := runCommand(t, deps, mise, npm, nil, "tools", "update")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tools["node"] != "24.18.0" {
		t.Fatalf("node pin not refreshed: %+v", cfg.Tools)
	}
	if cfg.OmpVersion != "18.1.22" {
		t.Fatalf("harness pin not refreshed: %q", cfg.OmpVersion)
	}
	if !strings.Contains(out, "node 24.0.0 -> 24.18.0") {
		t.Fatalf("update must report the change:\n%s", out)
	}
	if !strings.Contains(out, "python 3.12.14 is current") {
		t.Fatalf("update must report what did not change:\n%s", out)
	}
	before := out
	again := runCommand(t, deps, mise, &Recording{Replies: map[string]string{"@oh-my-pi/pi-coding-agent": "18.1.22"}}, nil, "tools", "update")
	if strings.Contains(again, "node 24.18.0 ->") {
		t.Fatalf("a second update must not re-report a change:\n%s\n%s", before, again)
	}
}

func TestApplySendsTheConvergenceCommandToTheBox(t *testing.T) {
	deps, _ := harness(t, "node = \"24.18.0\"\n", "18.1.22")
	boxSession := &session{}
	out := runCommand(t, deps, &Recording{}, &Recording{}, dialer{sessions: map[string]*session{"dev": boxSession}}, "tools", "apply", "dev")
	if len(boxSession.commands) != 1 {
		t.Fatalf("apply must run exactly one remote command: %+v", boxSession.commands)
	}
	script := boxSession.commands[0]
	for _, required := range []string{"set -eu", "mise use -g 'node@24.18.0'", "mise install", "npm install -g '@oh-my-pi/pi-coding-agent@18.1.22'"} {
		if !strings.Contains(script, required) {
			t.Fatalf("convergence script is missing %q:\n%s", required, script)
		}
	}
	if !strings.Contains(out, "converged dev") {
		t.Fatalf("apply must report the result:\n%s", out)
	}
}

func TestApplyDryRunPrintsWithoutOpeningASession(t *testing.T) {
	deps, _ := harness(t, "node = \"24.18.0\"\n", "18.1.22")
	deps.DryRun = true
	boxSession := &session{}
	out := runCommand(t, deps, &Recording{}, &Recording{}, dialer{sessions: map[string]*session{"dev": boxSession}}, "tools", "apply", "dev")
	if len(boxSession.commands) != 0 {
		t.Fatalf("dry run must not run anything on the box: %+v", boxSession.commands)
	}
	if !strings.Contains(out, "mise use -g 'node@24.18.0'") {
		t.Fatalf("dry run must print the command it would run:\n%s", out)
	}
}

func TestOutdatedListsOnlyDrift(t *testing.T) {
	deps, _ := harness(t, "node = \"24.0.0\"\npython = \"3.12.14\"\n", "18.1.22")
	mise := &Recording{Replies: map[string]string{"node": "24.18.0", "python": "3.12.14"}}
	npm := &Recording{Replies: map[string]string{"@oh-my-pi/pi-coding-agent": "18.1.22"}}
	out := runCommand(t, deps, mise, npm, nil, "tools", "outdated")
	if !strings.Contains(out, "node") || strings.Contains(out, "python") {
		t.Fatalf("outdated must list the pin that moved and nothing else:\n%s", out)
	}
}
