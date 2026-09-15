package packet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validInput() CreateInput {
	digest := strings.Repeat("ab", 32)
	baseline := strings.Repeat("cd", 32)
	return CreateInput{
		Box:                    "example-api",
		SourceManifestSHA256:   digest,
		BaselineManifestSHA256: &baseline,
		Task:                   "Fix the focused failing behavior.",
		CurrentState:           "What changed and what failed.",
		NextAction:             "Run the focused reproduction before editing.",
		Constraints:            []string{"Do not commit"},
	}
}

func TestPacketRejectsMissingRequiredFields(t *testing.T) {
	if _, err := Create(CreateInput{}); err == nil {
		t.Fatal("Create accepted an empty packet")
	}

	handoff, err := Create(validInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := handoff.Validate(); err != nil {
		t.Fatalf("Validate of a created packet: %v", err)
	}

	empty := ""
	cases := []struct {
		name   string
		mutate func(*Handoff)
	}{
		{"version", func(h *Handoff) { h.Version = 0 }},
		{"handoff_id", func(h *Handoff) { h.HandoffID = "" }},
		{"prepare_operation_id", func(h *Handoff) { h.PrepareOperationID = "" }},
		{"box", func(h *Handoff) { h.Box = "" }},
		{"source_manifest_sha256", func(h *Handoff) { h.SourceManifestSHA256 = "" }},
		{"task", func(h *Handoff) { h.Task = "" }},
		{"current_state", func(h *Handoff) { h.CurrentState = "" }},
		{"next_action", func(h *Handoff) { h.NextAction = "" }},
		{"created_at", func(h *Handoff) { h.CreatedAt = time.Time{} }},
		{"malformed uuid", func(h *Handoff) { h.HandoffID = "not-a-uuid" }},
		{"malformed digest", func(h *Handoff) { h.SourceManifestSHA256 = "zz" }},
		{"empty constraint", func(h *Handoff) { h.Constraints = []string{""} }},
		{"empty baseline digest", func(h *Handoff) { h.BaselineManifestSHA256 = &empty }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broken := handoff
			tc.mutate(&broken)
			if err := broken.Validate(); err == nil {
				t.Fatalf("Validate accepted a packet missing %s", tc.name)
			}
		})
	}
}

func TestPrepareRecordIsStableAndFsynced(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, ".env"), []byte("TOKEN=secret\n"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	sourceDest := openRoot(t, filepath.Join(t.TempDir(), "source"))
	baselineDest := openRoot(t, filepath.Join(t.TempDir(), "baseline"))
	recordDir := filepath.Join(t.TempDir(), "prepare")

	synced := 0
	prepared, err := Prepare(PrepareInput{
		SourceRoot:   source,
		SourceDest:   sourceDest,
		BaselineDest: baselineDest,
		RecordDir:    recordDir,
		Box:          "example-api",
		Task:         "Fix the focused failing behavior.",
		CurrentState: "What changed and what failed.",
		NextAction:   "Run the focused reproduction before editing.",
		Constraints:  []string{"Do not commit"},
		syncHook:     func() error { synced++; return nil },
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if synced == 0 {
		t.Fatal("prepare record was not fsynced")
	}
	if _, err := os.Lstat(filepath.Join(sourceDest.Name(), ".env")); err == nil {
		t.Fatal(".env entered the source tree")
	}
	if _, err := os.Lstat(filepath.Join(baselineDest.Name(), ".env")); err == nil {
		t.Fatal(".env entered the baseline tree")
	}

	first, err := os.ReadFile(filepath.Join(recordDir, "packet.json"))
	if err != nil {
		t.Fatalf("read packet: %v", err)
	}
	encoded, err := Encode(prepared.Handoff)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(first) != string(append(append([]byte{}, encoded...), '\n')) {
		t.Fatalf("on-disk packet is not the canonical encoding")
	}
	again, err := Encode(prepared.Handoff)
	if err != nil {
		t.Fatalf("Encode again: %v", err)
	}
	if string(encoded) != string(again) {
		t.Fatal("canonical packet encoding is not stable")
	}
	if prepared.Handoff.SourceManifestSHA256 == "" || prepared.Handoff.BaselineManifestSHA256 == nil {
		t.Fatal("packet did not bind both manifest digests")
	}
	if *prepared.Handoff.BaselineManifestSHA256 != prepared.Baseline.SHA256 {
		t.Fatalf("baseline digest %q does not match materialized baseline %q", *prepared.Handoff.BaselineManifestSHA256, prepared.Baseline.SHA256)
	}
	if prepared.Handoff.SourceManifestSHA256 != prepared.Source.SHA256 {
		t.Fatalf("source digest %q does not match materialized source %q", prepared.Handoff.SourceManifestSHA256, prepared.Source.SHA256)
	}
	if _, err := os.ReadFile(filepath.Join(recordDir, "record.json")); err != nil {
		t.Fatalf("read prepare record: %v", err)
	}
}

func openRoot(t *testing.T, path string) *os.Root {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatalf("open root %s: %v", path, err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}
