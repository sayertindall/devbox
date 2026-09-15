package cli

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func newTestFlags() *flag.FlagSet {
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.String("note", "", "a value flag")
	set.Bool("force", false, "a boolean flag")
	set.Bool("dry-run", false, "a boolean flag")
	return set
}

func TestParseAcceptsFlagsAfterPositionals(t *testing.T) {
	set := newTestFlags()
	positional, err := Parse(set, []string{"dev", "--note", "checked the instance"})
	if err != nil {
		t.Fatal(err)
	}
	if len(positional) != 1 || positional[0] != "dev" {
		t.Fatalf("positional arguments were lost: %+v", positional)
	}
	if set.Lookup("note").Value.String() != "checked the instance" {
		t.Fatalf("flag after a positional was not parsed: %q", set.Lookup("note").Value.String())
	}
}

func TestParseAcceptsFlagsBeforePositionals(t *testing.T) {
	set := newTestFlags()
	positional, err := Parse(set, []string{"--note=seen", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if len(positional) != 1 || positional[0] != "dev" {
		t.Fatalf("positional arguments were lost: %+v", positional)
	}
	if set.Lookup("note").Value.String() != "seen" {
		t.Fatalf("equals-form flag was not parsed: %q", set.Lookup("note").Value.String())
	}
}

func TestParseLeavesABooleanFlagAlone(t *testing.T) {
	set := newTestFlags()
	positional, err := Parse(set, []string{"--force", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if len(positional) != 1 || positional[0] != "dev" {
		t.Fatalf("a boolean flag must not swallow the next argument: %+v", positional)
	}
}

func TestParseStopsAtTheSeparator(t *testing.T) {
	set := newTestFlags()
	positional, err := Parse(set, []string{"dev", "--", "echo", "--not-a-flag"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"dev", "echo", "--not-a-flag"}
	if strings.Join(positional, " ") != strings.Join(want, " ") {
		t.Fatalf("arguments after the separator must pass through: %+v", positional)
	}
}

func TestParseRejectsAnUnknownFlag(t *testing.T) {
	set := newTestFlags()
	if _, err := Parse(set, []string{"--nope", "dev"}); err == nil {
		t.Fatal("an unknown flag must be an error rather than a silently ignored argument")
	}
}

func TestParseGlobalsKeepsTheRemoteCommandMarker(t *testing.T) {
	set := newTestFlags()
	command, err := ParseGlobals(set, []string{"exec", "dev", "--", "git", "log", "--oneline"})
	if err != nil {
		t.Fatal(err)
	}
	want := "exec dev -- git log --oneline"
	if strings.Join(command, " ") != want {
		t.Fatalf("the command line must reach the dispatcher unchanged:\n got %q\nwant %q", strings.Join(command, " "), want)
	}
}

func TestParseGlobalsStillTakesFlagsFromAnywhere(t *testing.T) {
	set := newTestFlags()
	command, err := ParseGlobals(set, []string{"machine", "new", "dev", "--note", "seen", "--force"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(command, " ") != "machine new dev" {
		t.Fatalf("global flags must be removed from the command line: %+v", command)
	}
	if set.Lookup("note").Value.String() != "seen" {
		t.Fatalf("flag value was not parsed: %q", set.Lookup("note").Value.String())
	}
	if set.Lookup("force").Value.String() != "true" {
		t.Fatalf("boolean flag was not parsed: %q", set.Lookup("force").Value.String())
	}
}

func TestParseGlobalsWithoutAMarkerIsJustTheCommand(t *testing.T) {
	set := newTestFlags()
	command, err := ParseGlobals(set, []string{"--dry-run", "machine", "list"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(command, " ") != "machine list" {
		t.Fatalf("unexpected command line: %+v", command)
	}
}

func TestParseGlobalsLeavesFlagsItDoesNotDefineToTheCommand(t *testing.T) {
	set := newTestFlags()
	// --help and --bucket belong to the command, not to the program, so they must
	// arrive where the command's own parser can see them.
	command, err := ParseGlobals(set, []string{"bootstrap", "upload", "--bucket", "gs://b"})
	if err != nil {
		t.Fatalf("a flag this program does not define must not be an error: %v", err)
	}
	want := "bootstrap upload --bucket gs://b"
	if strings.Join(command, " ") != want {
		t.Fatalf("the command line must pass through unchanged:\n got %q\nwant %q", strings.Join(command, " "), want)
	}
}
