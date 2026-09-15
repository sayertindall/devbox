package access

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"devbox/internal/gcloud"
)

// doctorReport is the report a box would send back, with the named checks failing.
func doctorReport(failing ...string) string {
	var report strings.Builder
	for _, name := range checkNames() {
		status := statusPass
		if slices.Contains(failing, name) {
			status = statusFail
		}
		fmt.Fprintf(&report, "%s%s%s%s%s the %s detail\n", name, reportTab, status, reportTab, name, name)
	}
	return report.String()
}

func TestDoctorReportsEveryCheckAndFailsWhenOneFails(t *testing.T) {
	session := &Recording{Reply: func(string) (string, error) { return doctorReport("egress", "docker-root"), nil }}
	deps, out, _ := testDeps(testConfig(), &gcloud.Fake{})
	err := commandFor(t, ops{open: openOn(session)}, "doctor").Run(context.Background(), deps, []string{"alpha"})
	if err == nil {
		t.Fatal("doctor must fail when a check fails")
	}
	if !strings.Contains(err.Error(), "egress") || !strings.Contains(err.Error(), "docker-root") {
		t.Errorf("error = %v, want the failing check names", err)
	}
	if strings.Contains(err.Error(), "cgroup-v2") {
		t.Errorf("error = %v, want only the names that failed", err)
	}
	printed := out.String()
	if !strings.Contains(printed, "FAIL egress") || !strings.Contains(printed, "PASS cgroup-v2") {
		t.Errorf("output =\n%s\nwant one PASS or FAIL line per check", printed)
	}
	if lines := strings.Count(printed, "\n"); lines != len(checkNames()) {
		t.Errorf("the report has %d lines, want %d", lines, len(checkNames()))
	}
}

func TestDoctorPassesWhenEveryCheckPasses(t *testing.T) {
	session := &Recording{Reply: func(string) (string, error) { return doctorReport(), nil }}
	deps, out, _ := testDeps(testConfig(), &gcloud.Fake{})
	if err := commandFor(t, ops{open: openOn(session)}, "doctor").Run(context.Background(), deps, []string{"alpha"}); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if len(session.Commands) != 1 {
		t.Fatalf("commands = %q, want one run of the checks", session.Commands)
	}
	if strings.Contains(out.String(), "FAIL") {
		t.Errorf("output =\n%s\nwant no failure", out.String())
	}
}

func TestDoctorFailsTheChecksABrokenRunNeverReported(t *testing.T) {
	lines := strings.Split(strings.TrimSuffix(doctorReport(), "\n"), "\n")
	truncated := strings.Join(lines[:2], "\n") + "\n"
	session := &Recording{Reply: func(string) (string, error) { return truncated, nil }}
	deps, out, _ := testDeps(testConfig(), &gcloud.Fake{})
	err := commandFor(t, ops{open: openOn(session)}, "doctor").Run(context.Background(), deps, []string{"alpha"})
	if err == nil {
		t.Fatal("a run that stopped early must not look like a run that passed")
	}
	for _, name := range checkNames()[2:] {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error = %v, want the check %s that never came back", err, name)
		}
	}
	if !strings.Contains(out.String(), "FAIL egress") {
		t.Errorf("output =\n%s\nwant the unreported checks named", out.String())
	}
}

func TestDoctorIgnoresWhatIsNotACheck(t *testing.T) {
	noise := "Warning: Permanently added 'alpha' (ED25519) to the list of known hosts.\n" + doctorReport()
	session := &Recording{Reply: func(string) (string, error) { return noise, nil }}
	deps, _, _ := testDeps(testConfig(), &gcloud.Fake{})
	if err := commandFor(t, ops{open: openOn(session)}, "doctor").Run(context.Background(), deps, []string{"alpha"}); err != nil {
		t.Fatalf("doctor: %v", err)
	}
}

// TestDoctorScriptReportsEveryCheck runs the generated script here, with the tools
// it calls replaced by stubs, and checks the one property devbox's parsing
// depends on: every check reports exactly one line and the script always exits
// zero. What each check answers is the box's business; this is about the shape of
// the report.
func TestDoctorScriptReportsEveryCheck(t *testing.T) {
	stubs := t.TempDir()
	for _, binary := range []string{"docker", "curl", "ldconfig", "infocmp", "omp", "gh", "gcloud"} {
		path := filepath.Join(stubs, binary)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(context.Background(), "sh", "-c", doctorScript(testConfig(), "alpha"))
	cmd.Env = append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the checks script exited nonzero: %v\n%s", err, out)
	}
	reported := map[string]int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(line, reportTab, 3)
		if len(fields) != 3 {
			continue
		}
		reported[fields[0]]++
	}
	names := checkNames()
	for _, name := range names {
		if reported[name] != 1 {
			t.Errorf("check %s reported %d times, want 1\n%s", name, reported[name], out)
		}
	}
	if len(reported) != len(names) {
		t.Errorf("the script reported %d checks, want %d\n%s", len(reported), len(names), out)
	}
}
