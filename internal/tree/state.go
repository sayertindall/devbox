package tree

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"devbox/internal/manifest"
)

// stateVersion is the tree state schema. A file written by a later devbox is
// refused rather than read with fields this build does not understand.
const stateVersion = 1

// State is one tree's recorded handoff between the local machine and one box:
// the digest of the projection devbox last sent to or received from the box, and
// where on the local machine that projection came from. It is what lets a pull
// refuse to apply over local work that has not been sent anywhere.
type State struct {
	Box       string `json:"box"`
	Tree      string `json:"tree"`
	Digest    string `json:"digest"`
	Path      string `json:"path"`
	UpdatedAt string `json:"updated_at"`
}

// stateFile is the whole of trees.json: one record per box and tree, not one per
// handoff, so the file cannot grow without bound.
type stateFile struct {
	Version int     `json:"version"`
	Trees   []State `json:"trees"`
}

// loadState reads the recorded handoffs. An absent file is an empty history, not
// an error: the first push of the first tree has nothing to read.
func loadState(path string) (stateFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return stateFile{Version: stateVersion}, nil
	}
	if err != nil {
		return stateFile{}, fmt.Errorf("read tree state %s: %w", path, err)
	}
	var file stateFile
	if err := json.Unmarshal(data, &file); err != nil {
		return stateFile{}, fmt.Errorf("decode tree state %s: %w", path, err)
	}
	if file.Version != stateVersion {
		return stateFile{}, fmt.Errorf("tree state %s has version %d, this devbox writes version %d", path, file.Version, stateVersion)
	}
	return file, nil
}

// find returns the record for one tree on one box.
func (f stateFile) find(name, tree string) (State, bool) {
	for _, entry := range f.Trees {
		if entry.Box == name && entry.Tree == tree {
			return entry, true
		}
	}
	return State{}, false
}

// record stores one handoff, replacing any earlier record for the same tree on
// the same box: the state file describes where the tree stands now, and the next
// push or pull compares against that.
func (f *stateFile) record(entry State) {
	for index := range f.Trees {
		if f.Trees[index].Box == entry.Box && f.Trees[index].Tree == entry.Tree {
			f.Trees[index] = entry
			return
		}
	}
	f.Trees = append(f.Trees, entry)
	slices.SortFunc(f.Trees, func(a, b State) int {
		if a.Box != b.Box {
			return strings.Compare(a.Box, b.Box)
		}
		return strings.Compare(a.Tree, b.Tree)
	})
}

// saveState stores the recorded handoffs atomically: a state file that is lost
// half written would make the next pull compare against a digest that was never
// handed over.
func saveState(path string, file stateFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory for %s: %w", path, err)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tree state: %w", err)
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

// newState is the record of one handoff.
func newState(name, tree, digest, path string, now time.Time) State {
	return State{
		Box:       name,
		Tree:      tree,
		Digest:    digest,
		Path:      path,
		UpdatedAt: now.UTC().Format(time.RFC3339),
	}
}

// writeFileAtomic installs bytes at path through a temporary file in the same
// directory: a reader sees either the previous file or the whole new one, and a
// writer that dies leaves no half-written state behind.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return fmt.Errorf("set permissions of %s: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
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
		return fmt.Errorf("open directory of %s: %w", path, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory of %s: %w", path, err)
	}
	return nil
}

// localManifest builds the manifest of the local tree, and is the only place a
// pull decides whether the local root is usable. A root that is absent is
// created when the operator has asked for the box tree to be applied over
// nothing; a root that exists but is not a directory is always refused.
func localManifest(root string, force bool) (manifest.Manifest, error) {
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !force {
			return manifest.Manifest{}, fmt.Errorf("local tree %s does not exist: create it, or pass --force to apply the tree from the box into it", root)
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return manifest.Manifest{}, fmt.Errorf("create local tree %s: %w", root, err)
		}
	case err != nil:
		return manifest.Manifest{}, fmt.Errorf("inspect local tree %s: %w", root, err)
	case !info.IsDir():
		return manifest.Manifest{}, fmt.Errorf("local tree %s is not a directory", root)
	}
	built, err := manifest.Build(root, manifest.Policy{})
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("build the manifest of %s: %w", root, err)
	}
	return built, nil
}
