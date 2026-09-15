// Baseline tests: what a sanitized baseline contains, what it must refuse, and
// what it must leave behind in the source repository (nothing).
//
// Every repository here is created locally with the Git CLI. No test needs a
// network remote, and no test shells out through an interpreter.
package baseline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"devbox/internal/manifest"
)

// git runs one Git command with an isolated configuration, so a developer's
// global config, hooks, or template directory cannot change a test outcome.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "init", "-q", "-b", "main")
}

// commit stages everything, including paths a default ignore rule would drop,
// so a test can put a secret or a vendored directory at HEAD on purpose.
func commit(t *testing.T, dir, message string) {
	t.Helper()
	git(t, dir, "add", "-A", "-f", ".")
	git(t, dir, "commit", "-q", "-m", message)
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// dest returns a trusted destination root plus its path, which is what the
// caller of Materialize is required to supply.
func dest(t *testing.T) (*os.Root, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dst")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir destination: %v", err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatalf("open destination root: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return root, path
}

func paths(m manifest.Manifest) []string {
	out := make([]string, 0, len(m.Entries))
	for _, e := range m.Entries {
		out = append(out, e.Path)
	}
	return out
}

func requireSame(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("baseline paths = %v, want %v", got, want)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func requireAbsent(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
		t.Fatalf("%s was materialized into the baseline", rel)
	}
}

func TestBaselineIncludesOnlyAllowedHEADPaths(t *testing.T) {
	source := t.TempDir()
	initRepo(t, source)
	write(t, source, "README.md", "readme\n")
	write(t, source, "main.go", "package main\n")
	write(t, source, "sub/a.txt", "a\n")
	write(t, source, "node_modules/pkg/index.js", "module.exports = 1\n")
	write(t, source, "dist/bundle.js", "bundled\n")
	commit(t, source, "initial")

	destination, destPath := dest(t)
	baseline, err := Materialize(source, manifest.Policy{}, destination)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	requireSame(t, paths(baseline), []string{"README.md", "main.go", "sub/a.txt"})
	if got := readFile(t, destPath, "sub/a.txt"); got != "a\n" {
		t.Fatalf("sub/a.txt = %q, want %q", got, "a\n")
	}
	requireAbsent(t, destPath, ".git")
	requireAbsent(t, destPath, "node_modules")
	requireAbsent(t, destPath, "dist")
}

func TestBaselineExcludesTrackedSecretAtHEAD(t *testing.T) {
	source := t.TempDir()
	initRepo(t, source)
	write(t, source, "main.go", "package main\n")
	write(t, source, ".env", "TOKEN=supersecret\n")
	write(t, source, "sub/.env.production", "TOKEN=alsosecret\n")
	write(t, source, ".ssh/id_ed25519", "PRIVATE KEY\n")
	commit(t, source, "initial")

	// The secrets really are at HEAD: the baseline must drop them anyway.
	if tracked := git(t, source, "ls-tree", "-r", "--name-only", "HEAD"); !strings.Contains(tracked, ".env") {
		t.Fatalf("test setup did not track .env at HEAD: %s", tracked)
	}

	destination, destPath := dest(t)
	baseline, err := Materialize(source, manifest.Policy{}, destination)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	requireSame(t, paths(baseline), []string{"main.go"})
	requireAbsent(t, destPath, ".env")
	requireAbsent(t, destPath, "sub/.env.production")
	requireAbsent(t, destPath, ".ssh/id_ed25519")
}

func TestSourceDeletionSurvivesBaselineOverlay(t *testing.T) {
	source := t.TempDir()
	initRepo(t, source)
	write(t, source, "keep.txt", "committed\n")
	write(t, source, "main.go", "package main\n")
	commit(t, source, "initial")

	// The working tree deletes a file that HEAD still has. The baseline is the
	// HEAD projection, so the deleted file must still be in it.
	if err := os.Remove(filepath.Join(source, "keep.txt")); err != nil {
		t.Fatalf("remove keep.txt: %v", err)
	}

	destination, destPath := dest(t)
	baseline, err := Materialize(source, manifest.Policy{}, destination)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	requireSame(t, paths(baseline), []string{"keep.txt", "main.go"})
	if got := readFile(t, destPath, "keep.txt"); got != "committed\n" {
		t.Fatalf("keep.txt = %q, want the HEAD content", got)
	}
}

func TestBaselineRejectsLFSSubmoduleAndNestedRepository(t *testing.T) {
	t.Run("git lfs", func(t *testing.T) {
		source := t.TempDir()
		initRepo(t, source)
		write(t, source, "main.go", "package main\n")
		write(t, source, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
		commit(t, source, "initial")

		destination, _ := dest(t)
		_, err := Materialize(source, manifest.Policy{}, destination)
		if err == nil {
			t.Fatal("Materialize accepted a Git LFS repository")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "lfs") {
			t.Fatalf("error = %v, want it to name LFS", err)
		}
	})

	t.Run("submodule", func(t *testing.T) {
		source := t.TempDir()
		initRepo(t, source)
		write(t, source, "main.go", "package main\n")
		commit(t, source, "initial")

		// A gitlink recorded directly, so no network remote is needed. This is
		// exactly what "git submodule add" leaves in the tree.
		head := strings.TrimSpace(git(t, source, "rev-parse", "HEAD"))
		git(t, source, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor/lib")
		write(t, source, ".gitmodules", "[submodule \"vendor/lib\"]\n\tpath = vendor/lib\n\turl = ../lib.git\n")
		git(t, source, "add", "-f", ".gitmodules")
		git(t, source, "commit", "-q", "-m", "add submodule")

		destination, _ := dest(t)
		_, err := Materialize(source, manifest.Policy{}, destination)
		if err == nil {
			t.Fatal("Materialize accepted a repository with a submodule")
		}
		if !strings.Contains(err.Error(), "submodule") {
			t.Fatalf("error = %v, want it to name the submodule", err)
		}
	})

	t.Run("nested repository", func(t *testing.T) {
		source := t.TempDir()
		initRepo(t, source)
		write(t, source, "main.go", "package main\n")
		commit(t, source, "initial")

		nested := filepath.Join(source, "vendor", "inner")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("mkdir nested: %v", err)
		}
		initRepo(t, nested)

		destination, _ := dest(t)
		_, err := Materialize(source, manifest.Policy{}, destination)
		if err == nil {
			t.Fatal("Materialize accepted a nested repository")
		}
		if !strings.Contains(err.Error(), "vendor/inner") {
			t.Fatalf("error = %v, want it to name the nested repository", err)
		}
	})
}

// A linked worktree's ".git" is a file, not a directory. The nested-repository
// scan must keep walking the rest of the source root after it, otherwise every
// entry sorted after ".git" escapes the scan entirely.
func TestBaselineScansPastRootWorktreeMarkerFile(t *testing.T) {
	origin := t.TempDir()
	initRepo(t, origin)
	write(t, origin, "main.go", "package main\n")
	commit(t, origin, "initial")

	source := filepath.Join(t.TempDir(), "linked")
	git(t, origin, "worktree", "add", "--detach", source, "HEAD")
	t.Cleanup(func() {
		git(t, origin, "worktree", "remove", "--force", source)
	})
	if info, err := os.Lstat(filepath.Join(source, ".git")); err != nil || info.IsDir() {
		t.Fatalf("test setup did not produce a worktree .git file: info=%v err=%v", info, err)
	}

	// "vendor" sorts after ".git", so a scan that stops at the root marker
	// never reaches this repository.
	nested := filepath.Join(source, "vendor", "inner")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	initRepo(t, nested)

	destination, _ := dest(t)
	_, err := Materialize(source, manifest.Policy{}, destination)
	if err == nil {
		t.Fatal("Materialize accepted a nested repository below a linked worktree")
	}
	if !strings.Contains(err.Error(), "vendor/inner") {
		t.Fatalf("error = %v, want it to name the nested repository", err)
	}
}

// Creating a worktree runs whatever clean/smudge filter the attributes declare,
// so a filter driver is refused before any checkout exists, not just the LFS one.
func TestBaselineRejectsCustomContentFilter(t *testing.T) {
	source := t.TempDir()
	initRepo(t, source)
	write(t, source, "main.go", "package main\n")
	write(t, source, ".gitattributes", "# comment filter=ignored\n\n*.secret filter=redact\n")
	commit(t, source, "initial")

	destination, _ := dest(t)
	_, err := Materialize(source, manifest.Policy{}, destination)
	if err == nil {
		t.Fatal("Materialize accepted a repository declaring a content filter")
	}
	if !strings.Contains(err.Error(), "redact") {
		t.Fatalf("error = %v, want it to name the filter driver", err)
	}
}

func TestNonGitProjectGetsSyntheticDiffBaseline(t *testing.T) {
	source := t.TempDir()
	write(t, source, "main.go", "package main\n")
	write(t, source, "sub/a.txt", "a\n")
	write(t, source, ".env", "TOKEN=secret\n")

	destination, destPath := dest(t)
	baseline, err := Materialize(source, manifest.Policy{}, destination)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	current, err := manifest.Build(source, manifest.Policy{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if baseline.SHA256 != current.SHA256 {
		t.Fatalf("synthetic baseline digest = %q, want the current source digest %q", baseline.SHA256, current.SHA256)
	}
	requireSame(t, paths(baseline), []string{"main.go", "sub/a.txt"})
	if got := readFile(t, destPath, "main.go"); got != "package main\n" {
		t.Fatalf("main.go = %q", got)
	}
	requireAbsent(t, destPath, ".env")
}

func TestBaselineRejectsGitWorktreeWithoutHEAD(t *testing.T) {
	source := t.TempDir()
	initRepo(t, source)
	write(t, source, "main.go", "package main\n")

	destination, destPath := dest(t)
	_, err := Materialize(source, manifest.Policy{}, destination)
	if err == nil {
		t.Fatal("Materialize accepted a Git worktree without a valid HEAD")
	}
	if !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("error = %v, want it to name HEAD", err)
	}
	// A refused source contributes nothing to the destination.
	requireAbsent(t, destPath, "main.go")
}

func TestBaselineDoesNotUseCurrentDirtyWorktreeContent(t *testing.T) {
	source := t.TempDir()
	initRepo(t, source)
	write(t, source, "main.go", "package main // committed\n")
	commit(t, source, "initial")

	write(t, source, "main.go", "package main // dirty\n")
	write(t, source, "untracked.txt", "untracked\n")

	destination, destPath := dest(t)
	baseline, err := Materialize(source, manifest.Policy{}, destination)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	requireSame(t, paths(baseline), []string{"main.go"})
	if got := readFile(t, destPath, "main.go"); got != "package main // committed\n" {
		t.Fatalf("main.go = %q, want the HEAD content", got)
	}
	requireAbsent(t, destPath, "untracked.txt")
}

func TestBaselineRemovesTemporaryWorktree(t *testing.T) {
	source := t.TempDir()
	initRepo(t, source)
	write(t, source, "main.go", "package main\n")
	commit(t, source, "initial")

	destination, _ := dest(t)
	if _, err := Materialize(source, manifest.Policy{}, destination); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	listing := git(t, source, "worktree", "list", "--porcelain")
	if count := strings.Count(listing, "worktree "); count != 1 {
		t.Fatalf("worktree list = %q, want only the main worktree", listing)
	}
	if _, err := os.Lstat(filepath.Join(source, ".git", "worktrees")); err == nil {
		t.Fatal("temporary worktree administrative directory was left behind")
	}
}
