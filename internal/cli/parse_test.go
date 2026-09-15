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
