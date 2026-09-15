package gcloud

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// script writes an executable that records its arguments and prints the given
// output. Runner is the one place devbox runs a real process, so its tests use a
// real process rather than a double.
func script(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gcloud")
	content := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDryRunPrintsTheCommandAndRunsNothing(t *testing.T) {
	var out bytes.Buffer
	// An executable that cannot exist proves nothing was run.
	runner := Runner{Executable: "/nonexistent/gcloud", DryRun: true, Output: &out}
	got, err := runner.Run(context.Background(), "compute", "instances", "list", "--project=p")
	if err != nil {
		t.Fatalf("a dry run must not fail: %v", err)
	}
	if got != "" {
		t.Fatalf("a dry run must produce no output: %q", got)
	}
	want := "/nonexistent/gcloud compute instances list --project=p\n"
	if out.String() != want {
		t.Fatalf("dry run printed\n got %q\nwant %q", out.String(), want)
	}
}

func TestRunReturnsOutputAndReportsAFailureWithItsDetail(t *testing.T) {
	runner := Runner{Executable: script(t, `printf 'boxes\n'`)}
	out, err := runner.Run(context.Background(), "compute", "instances", "list")
	if err != nil {
		t.Fatal(err)
	}
	if out != "boxes\n" {
		t.Fatalf("output was not returned: %q", out)
	}

	failing := Runner{Executable: script(t, `echo 'permission denied on resource' >&2; exit 2`)}
	_, err = failing.Run(context.Background(), "compute", "instances", "delete", "dev")
	if err == nil {
		t.Fatal("a failing command must be an error")
	}
	if !strings.Contains(err.Error(), "permission denied on resource") {
		t.Fatalf("the failure must carry what the tool said: %v", err)
	}
	if !strings.Contains(err.Error(), "compute instances delete dev") {
		t.Fatalf("the failure must name the command that failed: %v", err)
	}
}

func TestJSONAsksForJSONAndDecodesIt(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	runner := Runner{Executable: script(t, `echo "$@" > `+argsFile+`
printf '[{"name":"dev","status":"RUNNING"}]'`)}
	var facts []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := runner.JSON(context.Background(), &facts, "compute", "instances", "list"); err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Name != "dev" || facts[0].Status != "RUNNING" {
		t.Fatalf("result was not decoded: %+v", facts)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recorded), "--format=json") {
		t.Fatalf("JSON must ask the tool for JSON: %q", string(recorded))
	}
	if strings.Count(string(recorded), "--format=json") != 1 {
		t.Fatalf("--format must appear exactly once: %q", string(recorded))
	}
}

func TestJSONRefusesEmptyOutput(t *testing.T) {
	runner := Runner{Executable: script(t, `exit 0`)}
	var value any
	err := runner.JSON(context.Background(), &value, "compute", "instances", "describe", "dev")
	if err == nil {
		t.Fatal("an empty result must be an error rather than a zero value")
	}
	if !strings.Contains(err.Error(), "no output") {
		t.Fatalf("the error must say what happened: %v", err)
	}
}

func TestJSONReportsMalformedOutput(t *testing.T) {
	runner := Runner{Executable: script(t, `printf 'not json'`)}
	var value any
	if err := runner.JSON(context.Background(), &value, "compute", "instances", "list"); err == nil {
		t.Fatal("malformed output must be an error")
	}
}

func TestMissingRecognisesAnAbsentResource(t *testing.T) {
	cases := map[string]bool{
		"":                                       false,
		"ERROR: The resource 'x' was not found":  true,
		"ERROR: The resource could not be found": true,
		"ERROR: instance does not exist":         true,
		"ERROR: permission denied on resource (or it may not exist)": false,
	}
	for message, want := range cases {
		var err error
		if message != "" {
			err = errors.New(message)
		}
		if got := Missing(err); got != want {
			t.Fatalf("Missing(%q) = %t, want %t", message, got, want)
		}
	}
}

func TestFakeRecordsWhatWouldHaveRun(t *testing.T) {
	fake := &Fake{Reply: func(args []string) (string, error) { return "ok", nil }}
	if _, err := fake.Run(context.Background(), "compute", "instances", "create", "dev"); err != nil {
		t.Fatal(err)
	}
	if !fake.Ran("instances", "create", "dev") {
		t.Fatalf("the call was not recorded:\n%s", fake.Argv())
	}
	if fake.Last()[2] != "create" {
		t.Fatalf("Last returned the wrong call: %+v", fake.Last())
	}
}

func TestResourcesNormalizesBothShapes(t *testing.T) {
	object := `{"name":"dev"}`
	wrapped, err := Resources(object)
	if err != nil {
		t.Fatalf("a described resource must normalize: %v", err)
	}
	if string(wrapped) != `[`+object+`]` {
		t.Fatalf("Resources(%q) = %s, want a one-element array", object, wrapped)
	}
	list := `[{"name":"dev"},{"name":"other"}]`
	unchanged, err := Resources(list)
	if err != nil {
		t.Fatalf("a list must normalize: %v", err)
	}
	if string(unchanged) != list {
		t.Fatalf("Resources(%q) = %s, want it unchanged", list, unchanged)
	}
	empty, err := Resources("  ")
	if err != nil {
		t.Fatalf("empty output is an empty list: %v", err)
	}
	if string(empty) != "[]" {
		t.Fatalf("Resources of empty output = %s", empty)
	}
}

func TestNotJSONNamesTheMissingFlag(t *testing.T) {
	// gcloud's human table, which is what arrives when a decoded call forgot the
	// format flag. The message has to name the flag, not the parser's complaint.
	human := "allowSubnetCidrRoutesOverlap: false\ncreationTimestamp: 2026-09-15T16:20:18.166-07:00"
	if _, err := Resources(human); err == nil || !strings.Contains(err.Error(), "--format=json") {
		t.Fatalf("Resources of human output = %v, want an error naming --format=json", err)
	}
	if err := Object(human, &struct{}{}); err == nil || !strings.Contains(err.Error(), "--format=json") {
		t.Fatalf("Object of human output = %v, want an error naming --format=json", err)
	}
	if err := Object(`[{"name":"dev"}]`, &struct{}{}); err == nil {
		t.Fatal("a list where one object belongs must be an error, not a silent zero value")
	}
	if _, err := Resources(""); err != nil {
		t.Fatalf("empty output is not a failure: %v", err)
	}
}
