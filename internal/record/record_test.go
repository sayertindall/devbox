package record

import (
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "records"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestBeginWritesAPendingRecordBeforeTheCall(t *testing.T) {
	store := open(t)
	entry, err := store.Begin(KindCreate, "dev", []string{"compute", "instances", "create", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != StatePending {
		t.Fatalf("a record written before the call must be pending: %q", entry.State)
	}
	loaded, err := store.Load(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Args) != 4 || loaded.Args[0] != "compute" {
		t.Fatalf("the recorded argument vector was not preserved: %+v", loaded.Args)
	}
	if loaded.Box != "dev" || loaded.Kind != KindCreate {
		t.Fatalf("record does not identify what it was for: %+v", loaded)
	}
}

func TestUnresolvedBlocksOnlyItsOwnBox(t *testing.T) {
	store := open(t)
	pending, err := store.Begin(KindCreate, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Begin(KindSnapshot, "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Known(other, "snapshot-1"); err != nil {
		t.Fatal(err)
	}
	blocking, err := store.Unresolved("dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocking) != 1 || blocking[0].ID != pending.ID {
		t.Fatalf("the pending record must block its box: %+v", blocking)
	}
	if blocking, err := store.Unresolved("other"); err != nil || len(blocking) != 0 {
		t.Fatalf("a resolved record must not block: %+v %v", blocking, err)
	}
}

func TestUnknownBlocksUntilVerdictAndKeepsItsDetail(t *testing.T) {
	store := open(t)
	entry, err := store.Begin(KindSnapshot, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Unknown(entry, "connection closed"); err != nil {
		t.Fatal(err)
	}
	blocking, err := store.Unresolved("dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocking) != 1 || blocking[0].Detail != "connection closed" {
		t.Fatalf("an unknown outcome must block and keep the reason: %+v", blocking)
	}
	if err := store.Resolved(blocking[0], "snapshot exists"); err != nil {
		t.Fatal(err)
	}
	if blocking, err := store.Unresolved("dev"); err != nil || len(blocking) != 0 {
		t.Fatalf("a cleared record must stop blocking: %+v %v", blocking, err)
	}
}

func TestFailedRecordStopsBlocking(t *testing.T) {
	store := open(t)
	entry, err := store.Begin(KindCreate, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Failed(entry, "instance was not found"); err != nil {
		t.Fatal(err)
	}
	blocking, err := store.Unresolved("dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocking) != 0 {
		t.Fatalf("a call that provably did nothing must free the name: %+v", blocking)
	}
}

func TestRecordsSurviveARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := first.Begin(KindFork, "dev", []string{"compute", "machine-images", "create"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := second.Load(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != StatePending || loaded.Kind != KindFork {
		t.Fatalf("a reopened store lost the record: %+v", loaded)
	}
}

func TestLoadRejectsATraversalID(t *testing.T) {
	store := open(t)
	if _, err := store.Load("../../etc/passwd"); err == nil {
		t.Fatal("a record id must not reach outside the store")
	}
}

func TestRecordFileIsOwnerOnly(t *testing.T) {
	store := open(t)
	entry, err := store.Begin(KindCreate, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(store.Root, entry.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("records name cloud resources and must stay owner-only, got %o", info.Mode().Perm())
	}
}
