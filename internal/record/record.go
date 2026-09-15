// Package record keeps a durable note for every operation that can change or
// spend cloud resources.
//
// A create, fork, snapshot, or delete is written as pending, with the exact
// argument vector, before the call leaves the machine, and fsynced before the
// call so a dropped connection can never leave a billed resource that devbox
// cannot name. When the result is ambiguous the record becomes unknown and the
// CLI refuses the next mutation for that box until the operator reconciles it.
package record

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Kind names the operations devbox records.
const (
	KindCreate    = "create"
	KindFork      = "fork"
	KindSnapshot  = "snapshot"
	KindDelete    = "delete"
	KindBootstrap = "bootstrap"
)

// State is the certainty devbox has about an operation.
type State string

const (
	// StatePending means the call is in flight or its result was never seen.
	StatePending State = "pending"
	// StateKnown means the resource exists or the change is confirmed.
	StateKnown State = "known"
	// StateUnknown means the call failed in a way devbox cannot interpret. It
	// blocks the next mutation for the same box.
	StateUnknown State = "unknown"
)

// Record is one durable operation note.
type Record struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Box       string    `json:"box"`
	State     State     `json:"state"`
	Args      []string  `json:"args"`
	Result    string    `json:"result,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is the record directory.
type Store struct {
	Root string
}

// Open returns a store rooted at dir, creating it owner-only.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("record directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create record directory: %w", err)
	}
	return &Store{Root: dir}, nil
}

// Begin writes a pending record and fsyncs it before the caller acts.
func (s *Store) Begin(kind, box string, args []string) (Record, error) {
	if kind == "" || box == "" {
		return Record{}, fmt.Errorf("record kind and box are required")
	}
	now := time.Now().UTC()
	entry := Record{
		ID:        fmt.Sprintf("%s-%s-%d", kind, box, now.UnixNano()),
		Kind:      kind,
		Box:       box,
		State:     StatePending,
		Args:      append([]string{}, args...),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.write(entry); err != nil {
		return Record{}, err
	}
	return entry, nil
}

// Known marks an operation confirmed, naming the resource it produced.
func (s *Store) Known(entry Record, result string) error {
	entry.State = StateKnown
	entry.Result = result
	entry.Detail = ""
	entry.UpdatedAt = time.Now().UTC()
	return s.write(entry)
}

// Unknown marks an operation whose outcome devbox cannot interpret.
func (s *Store) Unknown(entry Record, detail string) error {
	entry.State = StateUnknown
	entry.Detail = detail
	entry.UpdatedAt = time.Now().UTC()
	return s.write(entry)
}

// Failed marks a pending operation that provably did nothing, so the next
// command is not blocked by it.
func (s *Store) Failed(entry Record, detail string) error {
	entry.State = StateKnown
	entry.Result = ""
	entry.Detail = detail
	entry.UpdatedAt = time.Now().UTC()
	return s.write(entry)
}

// Load reads one record by ID.
func (s *Store) Load(id string) (Record, error) {
	if strings.ContainsAny(id, `/\`) {
		return Record{}, fmt.Errorf("invalid record id")
	}
	data, err := os.ReadFile(filepath.Join(s.Root, id+".json"))
	if err != nil {
		return Record{}, fmt.Errorf("read record %s: %w", id, err)
	}
	var entry Record
	if err := json.Unmarshal(data, &entry); err != nil {
		return Record{}, fmt.Errorf("decode record %s: %w", id, err)
	}
	return entry, nil
}

// All returns every record, newest last.
func (s *Store) All() ([]Record, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read record directory: %w", err)
	}
	var out []Record
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		item, err := s.Load(id)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Unresolved returns the records that still block a mutation for a box: a
// pending or unknown operation whose outcome devbox never established.
func (s *Store) Unresolved(box string) ([]Record, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, entry := range all {
		if entry.Box != box {
			continue
		}
		if entry.State == StatePending || entry.State == StateUnknown {
			out = append(out, entry)
		}
	}
	return out, nil
}

// Resolved clears a blocking record after the operator has checked the cloud
// state by hand, recording why.
func (s *Store) Resolved(entry Record, detail string) error {
	entry.State = StateKnown
	entry.Detail = detail
	entry.UpdatedAt = time.Now().UTC()
	return s.write(entry)
}

// write stores one record atomically: temporary file, fsync, rename, fsync the
// directory. A record that is not durable is worse than no record at all.
func (s *Store) write(entry Record) error {
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return fmt.Errorf("create record directory: %w", err)
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	path := filepath.Join(s.Root, entry.ID+".json")
	file, err := os.CreateTemp(s.Root, ".record-*.tmp")
	if err != nil {
		return fmt.Errorf("create record temporary file: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("set record permissions: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("write record: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close record: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("install record: %w", err)
	}
	directory, err := os.Open(s.Root)
	if err != nil {
		return fmt.Errorf("open record directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync record directory: %w", err)
	}
	return nil
}
