package tree

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
)

// fixture is one test's world: a state directory devbox owns, a session standing
// in for a box, and what one command printed. Every command runs through the same
// openSession seam the binary uses, so a test exercises the command the registry
// dispatches rather than a copy of it.
type fixture struct {
	t       *testing.T
	state   string
	session access.Session
	dryRun  bool
	out     bytes.Buffer
	errOut  bytes.Buffer
}

func newFixture(t *testing.T, session access.Session) *fixture {
	t.Helper()
	state := t.TempDir()
	t.Setenv("DEVBOX_HOME", state)
	return &fixture{t: t, state: state, session: session}
}

// run executes one tree verb against the fixture's session.
func (f *fixture) run(argv ...string) error {
	f.t.Helper()
	previous := openSession
	openSession = func(context.Context, cli.Deps, box.Name) (access.Session, error) { return f.session, nil }
	f.t.Cleanup(func() { openSession = previous })

	f.out.Reset()
	f.errOut.Reset()
	deps := cli.Deps{Config: config.Default(), Out: &f.out, Err: &f.errOut, Stdin: strings.NewReader(""), DryRun: f.dryRun}
	return f.command(argv[0]).Run(context.Background(), deps, argv[1:])
}

// command returns the registered verb by name.
func (f *fixture) command(name string) cli.Command {
	f.t.Helper()
	for _, command := range Commands() {
		if command.Name == name {
			return command
		}
	}
	f.t.Fatalf("no %q command is registered", name)
	return cli.Command{}
}

// stateFile reads the recorded handoffs from the state directory devbox owns.
func (f *fixture) stateFile() stateFile {
	f.t.Helper()
	path, err := config.StatePath(stateFileName)
	if err != nil {
		f.t.Fatalf("state path: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatalf("read %s: %v", path, err)
	}
	var file stateFile
	if err := json.Unmarshal(data, &file); err != nil {
		f.t.Fatalf("decode %s: %v", path, err)
	}
	return file
}

// journal lists what is left in the journal directory devbox wrote for one tree
// on one box.
func (f *fixture) journal(name, tree string) []string {
	f.t.Helper()
	path, err := config.StatePath("journal", name, tree)
	if err != nil {
		f.t.Fatalf("state path: %v", err)
	}
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("read %s: %v", path, err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// staging records what a push staged at the moment it uploaded it. The staging
// directory is gone by the time the command returns, and what was in it is
// exactly what was allowed to leave the machine.
type staging struct {
	*access.Recording
	t *testing.T
	// entries is every path below a staged directory, relative and sorted.
	entries []string
	// files is the bytes of every uploaded regular file, by base name.
	files map[string][]byte
}

func newStaging(t *testing.T) *staging {
	return &staging{Recording: &access.Recording{}, t: t, files: map[string][]byte{}}
}

func (s *staging) Upload(ctx context.Context, local, remoteDir string) error {
	if err := s.Recording.Upload(ctx, local, remoteDir); err != nil {
		return err
	}
	info, err := os.Stat(local)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		if data, err := os.ReadFile(local); err == nil {
			s.files[filepath.Base(local)] = data
		}
		return nil
	}
	s.entries = pathsIn(s.t, local)
	return nil
}

// treeBox answers a download of one tree with the tree a test put on the box,
// the way the session copies it: into the download directory under the tree's own
// name.
type treeBox struct {
	*access.Recording
	t    *testing.T
	dir  string
	tree string
}

func (b *treeBox) Download(ctx context.Context, remote, localDir string) error {
	if err := b.Recording.Download(ctx, remote, localDir); err != nil {
		return err
	}
	return copyTree(b.t, b.dir, filepath.Join(localDir, b.tree))
}

// writeTree creates the entries a test names under root. A name that ends in a
// slash is a directory, and a content that begins with "-> " is a symbolic link
// to the rest.
func writeTree(t *testing.T, root string, entries map[string]string) {
	t.Helper()
	for name, content := range entries {
		full := filepath.Join(root, name)
		if strings.HasSuffix(name, "/") {
			mkdirAll(t, full)
			continue
		}
		mkdirAll(t, filepath.Dir(full))
		if target, linked := strings.CutPrefix(content, "-> "); linked {
			if err := os.Symlink(target, full); err != nil {
				t.Fatalf("link %s: %v", name, err)
			}
			continue
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// writeFile replaces the content of one file.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mkdirAll creates a directory a test names.
func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}

// pathsIn lists every path below root, relative and sorted, directories
// included: a staged tree is asserted whole, so a directory that should not be
// there is as visible as a file that should not be.
func pathsIn(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("list %s: %v", root, err)
	}
	sort.Strings(paths)
	return paths
}

// snapshot reads every regular file and symbolic link below root. A test compares
// a snapshot taken before a command with one taken after it to show that a tree
// is unchanged, excluded files included.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return nil
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			found[filepath.ToSlash(relative)] = "-> " + target
		case entry.Type().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			found[filepath.ToSlash(relative)] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return found
}

// copyTree copies every file and symbolic link of source into destination, which
// is what a test's box does when it answers a download.
func copyTree(t *testing.T, source, destination string) error {
	t.Helper()
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		full := filepath.Join(destination, relative)
		switch {
		case entry.IsDir():
			return os.MkdirAll(full, 0o755)
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			return os.Symlink(target, full)
		default:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(full, data, 0o644)
		}
	})
}

// treeFixture creates one working tree with the entries a test names. The
// directory's base name is the tree's name, which is also how a tree is named
// when the operator does not pass --tree.
func treeFixture(t *testing.T, name string, entries map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	writeTree(t, root, entries)
	return root
}
