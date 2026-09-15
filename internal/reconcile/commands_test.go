package reconcile

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"devbox/internal/cli"
	"devbox/internal/record"
)

// harness returns dependencies backed by a real record directory holding one
// pending, one unknown, and one resolved record.
func harness(t *testing.T) (cli.Deps, *record.Store, *bytes.Buffer, record.Record) {
	t.Helper()
	store, err := record.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.Begin(record.KindCreate, "dev", []string{"compute", "instances", "create", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := store.Begin(record.KindSnapshot, "dev", []string{"compute", "snapshots", "create", "dev-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Unknown(unknown, "connection closed before the reply"); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Begin(record.KindDelete, "other", []string{"compute", "instances", "delete", "other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Known(resolved, "deleted"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return cli.Deps{Records: store, Out: &out, Err: &out}, store, &out, pending
}

func invoke(t *testing.T, deps cli.Deps, args ...string) error {
	t.Helper()
	registry := cli.NewRegistry()
	registry.Add(Commands()...)
	return registry.Run(context.Background(), deps, args)
}

func TestListShowsUnresolvedRecordsWithTheirCommand(t *testing.T) {
	deps, _, out, pending := harness(t)
	if err := invoke(t, deps, "reconcile"); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, pending.ID) {
		t.Fatalf("a pending record must be listed:\n%s", text)
	}
	if !strings.Contains(text, "compute instances create dev") {
		t.Fatalf("the recorded command must be shown so the operator can check it:\n%s", text)
	}
	if !strings.Contains(text, "connection closed before the reply") {
		t.Fatalf("the failure detail must be shown:\n%s", text)
	}
	if strings.Contains(text, "other") {
		t.Fatalf("a resolved record must not be listed:\n%s", text)
	}
}

func TestListFiltersByBox(t *testing.T) {
	deps, _, out, _ := harness(t)
	if err := invoke(t, deps, "reconcile", "--box", "elsewhere"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no unresolved records") {
		t.Fatalf("an unrelated box must report nothing to reconcile:\n%s", out.String())
	}
}

func TestClearRequiresANote(t *testing.T) {
	deps, store, _, pending := harness(t)
	if err := invoke(t, deps, "reconcile", pending.ID); err == nil {
		t.Fatal("clearing a record without saying what was checked must fail")
	}
	after, err := store.Load(pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != record.StatePending {
		t.Fatalf("a refused clear changed the record: %+v", after)
	}
}

func TestClearRecordsTheVerdict(t *testing.T) {
	deps, store, out, pending := harness(t)
	if err := invoke(t, deps, "reconcile", pending.ID, "--note", "instance exists and is running"); err != nil {
		t.Fatal(err)
	}
	after, err := store.Load(pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != record.StateKnown {
		t.Fatalf("record was not cleared: %+v", after)
	}
	if after.Detail != "instance exists and is running" {
		t.Fatalf("verdict was not recorded: %q", after.Detail)
	}
	if !strings.Contains(out.String(), "cleared") {
		t.Fatalf("clear must report what it did:\n%s", out.String())
	}
	out.Reset()
	if err := invoke(t, deps, "reconcile", pending.ID, "--note", "again"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already resolved") {
		t.Fatalf("a second clear must be reported as a no-op:\n%s", out.String())
	}
}
