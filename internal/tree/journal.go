package tree

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"devbox/internal/manifest"
)

// journalFiles is the directory inside a journal that holds the saved bytes.
const journalFiles = "files"

// journal is the rollback record of one pull: the manifest the local tree had
// before the apply, the directories it had, and the bytes of every local file the
// apply can destroy. Nothing else is copied, and no excluded path can be in it,
// because both manifests it is built from come from the allowlist walk: a path
// both manifests declare with the same bytes cannot be lost, and a path neither
// declares is never named.
type journal struct {
	dir    string
	before manifest.Manifest
	dirs   []string
}

// journalIndex is what a journal keeps beside the saved bytes.
type journalIndex struct {
	// Manifest is the state the local tree has to be put back to.
	Manifest manifest.Manifest `json:"manifest"`
	// Dirs lists every directory the local tree had before the apply, so a
	// rollback can remove the directories the apply created and leave the rest.
	Dirs []string `json:"dirs"`
}

// newJournal writes the rollback record of one apply before that apply starts. A
// journal that is not on disk before the first byte is written is no journal at
// all, so the index and every saved file are flushed here.
func newJournal(dir, root string, before, arrived manifest.Manifest) (*journal, error) {
	saved := filepath.Join(dir, journalFiles)
	if err := os.MkdirAll(saved, 0o700); err != nil {
		return nil, fmt.Errorf("create the rollback journal %s: %w", dir, err)
	}
	dirs, err := localDirs(root)
	if err != nil {
		return nil, err
	}
	index, err := json.MarshalIndent(journalIndex{Manifest: before, Dirs: dirs}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the rollback journal %s: %w", dir, err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "index.json"), append(index, '\n'), 0o600); err != nil {
		return nil, err
	}
	arrivedByPath := entriesByPath(arrived)
	for _, entry := range before.Entries {
		if entry.Kind != manifest.KindFile {
			continue
		}
		if current, retained := arrivedByPath[entry.Path]; retained && current == entry {
			continue
		}
		target := filepath.Join(saved, entry.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, fmt.Errorf("create %s in the rollback journal: %w", filepath.Dir(entry.Path), err)
		}
		if err := copyFileBytes(filepath.Join(root, entry.Path), target, 0o600); err != nil {
			return nil, fmt.Errorf("save %s for rollback: %w", entry.Path, err)
		}
	}
	return &journal{dir: dir, before: before, dirs: dirs}, nil
}

// discard removes the journal. Only a caller whose apply is verified whole may
// call it: while the local tree has not been shown to match what was intended,
// the journal is the only copy of the bytes it holds.
func (j *journal) discard() error {
	if err := os.RemoveAll(j.dir); err != nil {
		return fmt.Errorf("remove the rollback journal %s: %w", j.dir, err)
	}
	return nil
}

// restore puts the local tree back to the state the journal recorded.
//
// It undoes the apply in two passes. First every removal: a path whose bytes the
// apply changed, and a path the apply added, both go. A path is only removed when
// what is there now is exactly what the arrived manifest declared, so a path the
// apply never reached is never touched. Then the directories the apply created go
// while they are empty, which is what lets a recorded file come back at a path the
// apply turned into a directory. Only then are the recorded entries written back,
// from the saved bytes and the recorded targets. The journal is removed only after
// the tree has been read back and compared against the recorded manifest.
func (j *journal) restore(root string, arrived manifest.Manifest) error {
	current, err := manifest.Build(root, manifest.Policy{})
	if err != nil {
		return fmt.Errorf("read the local tree back: %w", err)
	}
	here := entriesByPath(current)
	recorded := entriesByPath(j.before)
	landed := entriesByPath(arrived)

	var lost []string
	for _, path := range unionPaths(recorded, landed) {
		want, wanted := recorded[path]
		found, present := here[path]
		switch {
		case wanted && present && found == want:
		case wanted:
			if present {
				if err := os.Remove(filepath.Join(root, path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("remove %s: %w", path, err)
				}
			}
			lost = append(lost, path)
		case present && found == landed[path]:
			// A path only the arrived manifest declared, in exactly the state it
			// declared: the apply created it, so the rollback removes it.
			if err := os.Remove(filepath.Join(root, path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove the added path %s: %w", path, err)
			}
		}
	}

	if err := j.removeCreatedDirs(root); err != nil {
		return err
	}
	for _, path := range lost {
		if err := j.putBack(root, recorded[path]); err != nil {
			return err
		}
	}

	after, err := manifest.Build(root, manifest.Policy{})
	if err != nil {
		return fmt.Errorf("read the restored tree back: %w", err)
	}
	if err := manifest.Compare(j.before, after); err != nil {
		return fmt.Errorf("the local tree does not match the state the journal recorded: %w", err)
	}
	return j.discard()
}

// putBack writes one recorded entry into a local tree that no longer has it: a
// file from the saved bytes, a symbolic link from the target the manifest
// recorded.
func (j *journal) putBack(root string, entry manifest.Entry) error {
	target := filepath.Join(root, entry.Path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create the directory of %s: %w", entry.Path, err)
	}
	switch entry.Kind {
	case manifest.KindSymlink:
		if err := os.Symlink(entry.Target, target); err != nil {
			return fmt.Errorf("restore the symbolic link %s: %w", entry.Path, err)
		}
		return nil
	case manifest.KindFile:
		mode := os.FileMode(0o644)
		if entry.Executable {
			mode = 0o755
		}
		if err := copyFileBytes(filepath.Join(j.dir, journalFiles, entry.Path), target, mode); err != nil {
			return fmt.Errorf("restore %s: %w", entry.Path, err)
		}
		return nil
	}
	return fmt.Errorf("restore %s: unsupported kind %q", entry.Path, entry.Kind)
}

// removeCreatedDirs removes the directories the apply created, deepest first and
// only while they are empty. A directory that was there before the apply, and a
// directory that holds anything at all, is left alone.
func (j *journal) removeCreatedDirs(root string) error {
	before := make(map[string]bool, len(j.dirs))
	for _, dir := range j.dirs {
		before[dir] = true
	}
	now, err := localDirs(root)
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(now)))
	for _, dir := range now {
		if before[dir] {
			continue
		}
		// A directory that still holds something cannot be one the apply created
		// on its own, and it is not devbox's to empty: it stays, and the
		// comparison at the end of the restore decides whether what is left
		// behind is a difference the journal has to account for.
		os.Remove(filepath.Join(root, dir))
	}
	return nil
}

// unionPaths lists every path either manifest declares, which is exactly the set
// of paths an apply can have changed.
func unionPaths(first, second map[string]manifest.Entry) []string {
	seen := make(map[string]bool, len(first)+len(second))
	paths := make([]string, 0, len(first)+len(second))
	for _, index := range []map[string]manifest.Entry{first, second} {
		for path := range index {
			if seen[path] {
				continue
			}
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// localDirs lists every directory below root, relative and normalized. It does
// not follow symbolic links, so a link to a directory is not one of them.
func localDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() || path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dirs = append(dirs, relative)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list the directories below %s: %w", root, err)
	}
	return dirs, nil
}

// copyFileBytes writes source's bytes to destination and gives it mode. The
// journal uses it to save one file and the rollback to put it back; it flushes
// the copy, because a saved file that is not on disk is not a rollback.
func copyFileBytes(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
