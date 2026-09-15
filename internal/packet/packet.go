// Package packet builds the immutable handoff written before any transfer
// (REQUIREMENTS FR-002, FR-003).
package packet

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"devbox/internal/baseline"
	"devbox/internal/box"
	"devbox/internal/manifest"
)

const Version = 1

type Handoff struct {
	Version                int       `json:"version"`
	HandoffID              string    `json:"handoff_id"`
	PrepareOperationID     string    `json:"prepare_operation_id"`
	Box                    string    `json:"box"`
	SourceManifestSHA256   string    `json:"source_manifest_sha256"`
	BaselineManifestSHA256 *string   `json:"baseline_manifest_sha256"`
	BaseRevision           *string   `json:"base_revision"`
	Task                   string    `json:"task"`
	CurrentState           string    `json:"current_state"`
	NextAction             string    `json:"next_action"`
	Constraints            []string  `json:"constraints"`
	CreatedAt              time.Time `json:"created_at"`
}

type CreateInput struct {
	Box                    string
	SourceManifestSHA256   string
	BaselineManifestSHA256 *string
	BaseRevision           *string
	Task                   string
	CurrentState           string
	NextAction             string
	Constraints            []string
}

type PrepareInput struct {
	SourceRoot   string
	Policy       manifest.Policy
	SourceDest   *os.Root
	BaselineDest *os.Root
	RecordDir    string
	Box          string
	Task         string
	CurrentState string
	NextAction   string
	Constraints  []string
	BaseRevision *string
	syncHook     func() error
}

type Prepared struct {
	Handoff  Handoff
	Source   manifest.Manifest
	Baseline manifest.Manifest
}

type Record struct {
	PacketSHA256 string  `json:"packet_sha256"`
	Handoff      Handoff `json:"handoff"`
}

func Create(input CreateInput) (Handoff, error) {
	handoffID, err := newUUID()
	if err != nil {
		return Handoff{}, err
	}
	operationID, err := newUUID()
	if err != nil {
		return Handoff{}, err
	}
	constraints := input.Constraints
	if constraints == nil {
		constraints = []string{}
	}
	handoff := Handoff{
		Version:                Version,
		HandoffID:              handoffID,
		PrepareOperationID:     operationID,
		Box:                    input.Box,
		SourceManifestSHA256:   input.SourceManifestSHA256,
		BaselineManifestSHA256: input.BaselineManifestSHA256,
		BaseRevision:           input.BaseRevision,
		Task:                   input.Task,
		CurrentState:           input.CurrentState,
		NextAction:             input.NextAction,
		Constraints:            constraints,
		CreatedAt:              time.Now().UTC(),
	}
	if err := handoff.Validate(); err != nil {
		return Handoff{}, err
	}
	return handoff, nil
}

func (h Handoff) Validate() error {
	if h.Version != Version {
		return fmt.Errorf("unsupported packet version %d", h.Version)
	}
	if !isUUID(h.HandoffID) {
		return fmt.Errorf("invalid handoff ID")
	}
	if !isUUID(h.PrepareOperationID) {
		return fmt.Errorf("invalid prepare operation ID")
	}
	if _, err := box.ParseName(h.Box); err != nil {
		return fmt.Errorf("invalid box name: %w", err)
	}
	if !isDigest(h.SourceManifestSHA256) {
		return fmt.Errorf("invalid source manifest digest")
	}
	if h.BaselineManifestSHA256 != nil && !isDigest(*h.BaselineManifestSHA256) {
		return fmt.Errorf("invalid baseline manifest digest")
	}
	if h.BaseRevision != nil && strings.TrimSpace(*h.BaseRevision) == "" {
		return fmt.Errorf("base revision is empty")
	}
	if strings.TrimSpace(h.Task) == "" {
		return fmt.Errorf("task is required")
	}
	if strings.TrimSpace(h.CurrentState) == "" {
		return fmt.Errorf("current state is required")
	}
	if strings.TrimSpace(h.NextAction) == "" {
		return fmt.Errorf("next action is required")
	}
	if h.Constraints == nil {
		return fmt.Errorf("constraints are required")
	}
	for _, constraint := range h.Constraints {
		if strings.TrimSpace(constraint) == "" {
			return fmt.Errorf("constraint is empty")
		}
	}
	if h.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	return nil
}

func Encode(h Handoff) ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	if !utf8.ValidString(h.Task) || !utf8.ValidString(h.CurrentState) || !utf8.ValidString(h.NextAction) {
		return nil, fmt.Errorf("packet text is not valid UTF-8")
	}
	encoded, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("encode packet: %w", err)
	}
	return encoded, nil
}

func Digest(h Handoff) (string, error) {
	encoded, err := Encode(h)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func Prepare(input PrepareInput) (Prepared, error) {
	if input.SourceDest == nil || input.BaselineDest == nil {
		return Prepared{}, fmt.Errorf("source and baseline destination roots are required")
	}
	if input.RecordDir == "" {
		return Prepared{}, fmt.Errorf("prepare record directory is required")
	}

	sourceManifest, err := manifest.Build(input.SourceRoot, input.Policy)
	if err != nil {
		return Prepared{}, fmt.Errorf("build source manifest: %w", err)
	}
	if err := manifest.Materialize(input.SourceRoot, sourceManifest, input.SourceDest); err != nil {
		return Prepared{}, fmt.Errorf("materialize source: %w", err)
	}

	baselineManifest, err := baseline.Materialize(input.SourceRoot, input.Policy, input.BaselineDest)
	if err != nil {
		return Prepared{}, fmt.Errorf("materialize baseline: %w", err)
	}
	baselineDigest := baselineManifest.SHA256

	handoff, err := Create(CreateInput{
		Box:                    input.Box,
		SourceManifestSHA256:   sourceManifest.SHA256,
		BaselineManifestSHA256: &baselineDigest,
		BaseRevision:           input.BaseRevision,
		Task:                   input.Task,
		CurrentState:           input.CurrentState,
		NextAction:             input.NextAction,
		Constraints:            input.Constraints,
	})
	if err != nil {
		return Prepared{}, err
	}

	packetBytes, err := Encode(handoff)
	if err != nil {
		return Prepared{}, err
	}
	packetDigest, err := Digest(handoff)
	if err != nil {
		return Prepared{}, err
	}
	recordBytes, err := json.Marshal(Record{PacketSHA256: packetDigest, Handoff: handoff})
	if err != nil {
		return Prepared{}, fmt.Errorf("encode prepare record: %w", err)
	}
	sourceBytes, err := manifest.Encode(sourceManifest)
	if err != nil {
		return Prepared{}, err
	}
	baselineBytes, err := manifest.Encode(baselineManifest)
	if err != nil {
		return Prepared{}, err
	}

	if err := os.MkdirAll(input.RecordDir, 0o700); err != nil {
		return Prepared{}, fmt.Errorf("create prepare directory: %w", err)
	}
	writes := []struct {
		name string
		data []byte
	}{
		{"source-manifest.json", sourceBytes},
		{"baseline-manifest.json", baselineBytes},
		{"packet.json", packetBytes},
		{"record.json", recordBytes},
	}
	for _, file := range writes {
		if err := writeAtomic(filepath.Join(input.RecordDir, file.name), file.data); err != nil {
			return Prepared{}, err
		}
	}
	if input.syncHook != nil {
		if err := input.syncHook(); err != nil {
			return Prepared{}, fmt.Errorf("prepare sync hook: %w", err)
		}
	}
	return Prepared{Handoff: handoff, Source: sourceManifest, Baseline: baselineManifest}, nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".packet-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("set permissions for %s: %w", path, err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open directory for %s: %w", path, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory for %s: %w", path, err)
	}
	return nil
}

func newUUID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(bytes[0:4]), hex.EncodeToString(bytes[4:6]), hex.EncodeToString(bytes[6:8]), hex.EncodeToString(bytes[8:10]), hex.EncodeToString(bytes[10:16])), nil
}

func isUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' || value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b' {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !isHex(char) {
			return false
		}
	}
	return true
}

func isDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if !isHex(char) {
			return false
		}
	}
	return true
}

func isHex(char rune) bool {
	return char >= '0' && char <= '9' || char >= 'a' && char <= 'f'
}
