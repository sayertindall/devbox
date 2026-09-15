// Package baseline produces the sanitized baseline a return projection is
// diffed against (REQUIREMENTS FR-003, NFR-SEC-003, INV-003).
//
// The baseline is what the source looked like at HEAD, filtered through the
// same positive allowlist a source projection uses. It is never the current
// working tree: a file the developer deleted or edited locally must still be in
// the baseline with its HEAD content, otherwise the diff would report the
// server as having created or changed a file it never touched.
//
// HEAD content is obtained by asking Git for a throwaway detached worktree and
// building the manifest from that ordinary directory. Nothing parses Git object
// storage, nothing copies .git, refs, objects, or bundles, and no Git command is
// ever built by string interpolation: every invocation is exec.Command with
// separate arguments, so a branch name, path, or ref can never become a flag or
// a shell word.
package baseline

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"devbox/internal/manifest"
)

// gitMarker is the repository marker at a source root: a directory in an
// ordinary checkout, a file in a worktree checkout.
const gitMarker = ".git"

// skipDirs are directory names the nested-repository scan does not descend
// into. It mirrors the directory names in the manifest package's mandatory
// exclude list: those subtrees never enter a projection, so a repository buried
// inside one is irrelevant and must not cause a false rejection.
//
// The list is duplicated rather than exported from manifest: exporting it would
// widen a reviewed security API for one scan. Revisit if a third caller needs
// the same list.
var skipDirs = []string{".ssh", ".aws", ".config", ".claude", ".codex", ".omp", ".agentbox", "node_modules", "dist", "build"}

// Materialize writes the sanitized HEAD baseline of sourceRoot into the
// caller's already-trusted destination root and returns the manifest describing
// exactly what it wrote.
//
// The destination is a pre-opened *os.Root rather than a path for the same
// reason manifest.Materialize requires one: only the caller can prove which
// directory it approved, and a path string would be re-resolved here through
// ancestors that may have been replaced in the meantime.
func Materialize(sourceRoot string, policy manifest.Policy, destination *os.Root) (manifest.Manifest, error) {
	isRepo, err := hasGitMarker(sourceRoot)
	if err != nil {
		return manifest.Manifest{}, err
	}
	if !isRepo {
		return synthetic(sourceRoot, policy, destination)
	}
	return fromHEAD(sourceRoot, policy, destination)
}

// synthetic is the baseline for a source with no history: the current source
// projection itself. A diff against it reports every server change and no
// phantom ones, which is exactly what a project without Git can offer.
func synthetic(sourceRoot string, policy manifest.Policy, destination *os.Root) (manifest.Manifest, error) {
	m, err := manifest.Build(sourceRoot, policy)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("build synthetic baseline: %w", err)
	}
	if err := manifest.Materialize(sourceRoot, m, destination); err != nil {
		return manifest.Manifest{}, fmt.Errorf("materialize synthetic baseline: %w", err)
	}
	return m, nil
}

// fromHEAD builds the baseline from a temporary detached worktree at HEAD.
//
// The named return values exist for the cleanup defer: a worktree this function
// created and could not remove is a leak in the developer's own repository, so
// it is reported rather than swallowed, even on an otherwise successful build.
func fromHEAD(sourceRoot string, policy manifest.Policy, destination *os.Root) (m manifest.Manifest, err error) {
	if err := requireHEAD(sourceRoot); err != nil {
		return manifest.Manifest{}, err
	}
	// Refused before any worktree is created: none of these can be projected
	// correctly, and creating a checkout first would only add cleanup risk.
	if err := rejectUnsupportedContent(sourceRoot); err != nil {
		return manifest.Manifest{}, err
	}

	parent, err := os.MkdirTemp("", "agentbox-baseline-")
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("create baseline workspace: %w", err)
	}
	// git worktree add wants to create the leaf itself.
	worktree := filepath.Join(parent, "head")

	if out, addErr := runGit(sourceRoot, "worktree", "add", "--detach", worktree, "HEAD"); addErr != nil {
		os.RemoveAll(parent)
		return manifest.Manifest{}, fmt.Errorf("create baseline worktree: %w: %s", addErr, out)
	}
	defer func() {
		if cleanupErr := cleanup(sourceRoot, parent, worktree); cleanupErr != nil {
			// A leaked worktree invalidates the result: the caller must see the
			// path so it can be removed, not a baseline that looks complete.
			m = manifest.Manifest{}
			err = errors.Join(err, cleanupErr)
		}
	}()

	// The worktree is now an ordinary directory tree, so the reviewed manifest
	// API applies unchanged: its root .git marker file is excluded by policy,
	// and no object storage exists inside it to copy.
	baseline, err := manifest.Build(worktree, policy)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("build baseline manifest: %w", err)
	}
	if err := manifest.Materialize(worktree, baseline, destination); err != nil {
		return manifest.Manifest{}, fmt.Errorf("materialize baseline: %w", err)
	}
	return baseline, nil
}

// cleanup removes the temporary worktree and the administrative record Git
// keeps for it. Every failure names the leftover path.
func cleanup(sourceRoot, parent, worktree string) error {
	if out, err := runGit(sourceRoot, "worktree", "remove", "--force", worktree); err != nil {
		return fmt.Errorf("remove temporary baseline worktree %s: %w: %s", worktree, err, out)
	}
	if out, err := runGit(sourceRoot, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune temporary baseline worktree %s: %w: %s", worktree, err, out)
	}
	if err := os.RemoveAll(parent); err != nil {
		return fmt.Errorf("remove temporary baseline workspace %s: %w", parent, err)
	}
	return nil
}

// hasGitMarker reports whether sourceRoot is the top of a Git checkout.
//
// The marker is checked instead of asking Git for a toplevel, so a source root
// that is merely a subdirectory of some repository is treated as having no
// history and gets a synthetic baseline. That is the conservative answer: the
// alternative would silently project a whole enclosing repository the caller
// never named.
func hasGitMarker(sourceRoot string) (bool, error) {
	info, err := os.Lstat(sourceRoot)
	if err != nil {
		return false, fmt.Errorf("stat source root: %w", err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("source root %s is not a directory", sourceRoot)
	}
	if _, err := os.Lstat(filepath.Join(sourceRoot, gitMarker)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat repository marker: %w", err)
	}
	return true, nil
}

// requireHEAD refuses a checkout whose HEAD does not resolve to a commit: an
// unborn branch, a corrupt reference, or a directory that only looks like a
// repository. Such a source has no history to base a baseline on, and guessing
// one would make every returned file look new.
func requireHEAD(sourceRoot string) error {
	out, err := runGit(sourceRoot, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(out) == "" {
		return fmt.Errorf("source %s is a Git worktree without a valid HEAD commit", sourceRoot)
	}
	return nil
}

// rejectUnsupportedContent refuses the source shapes whose HEAD cannot be
// projected honestly: content filters including Git LFS (checkout runs a driver
// and yields something other than the recorded bytes), submodules (HEAD records
// a commit, not a tree, and a partial copy is forbidden), and nested
// repositories.
func rejectUnsupportedContent(sourceRoot string) error {
	if err := rejectFiltersAndSubmodules(sourceRoot); err != nil {
		return err
	}
	return rejectNestedRepositories(sourceRoot)
}

// rejectFiltersAndSubmodules inspects the HEAD tree itself, so it sees what a
// checkout would produce rather than what the working tree happens to hold.
func rejectFiltersAndSubmodules(sourceRoot string) error {
	out, err := runGit(sourceRoot, "ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return fmt.Errorf("list HEAD tree: %w: %s", err, out)
	}
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		// "<mode> <type> <object>\t<path>"
		meta, entryPath, found := strings.Cut(record, "\t")
		if !found {
			return fmt.Errorf("unexpected HEAD tree record %q", record)
		}
		fields := strings.Fields(meta)
		if len(fields) < 2 {
			return fmt.Errorf("unexpected HEAD tree record %q", record)
		}
		if fields[1] == "commit" {
			return fmt.Errorf("submodule at %s is not allowed in source", entryPath)
		}
		switch path.Base(entryPath) {
		case ".lfsconfig":
			return fmt.Errorf("Git LFS configuration at %s is not allowed in source", entryPath)
		case ".gitattributes":
			if err := rejectFilterAttributes(sourceRoot, entryPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// rejectFilterAttributes reads one .gitattributes blob at HEAD and refuses any
// content filter declared in it.
//
// This is not only about LFS. "git worktree add" checks files out through
// whatever clean/smudge driver the attributes name, and that driver is an
// arbitrary command taken from the developer's Git configuration. A baseline
// must be the recorded HEAD bytes, produced without running project-supplied
// programs, so any "filter=<driver>" is refused before a checkout exists. LFS
// keeps its own wording because it is the case a developer will actually hit.
func rejectFilterAttributes(sourceRoot, entryPath string) error {
	out, err := runGit(sourceRoot, "cat-file", "blob", "HEAD:"+entryPath)
	if err != nil {
		return fmt.Errorf("read %s at HEAD: %w: %s", entryPath, err, out)
	}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, field := range strings.Fields(line) {
			driver, declared := strings.CutPrefix(field, "filter=")
			if !declared || driver == "" {
				continue
			}
			if strings.EqualFold(driver, "lfs") {
				return fmt.Errorf("Git LFS tracking declared in %s is not allowed in source", entryPath)
			}
			return fmt.Errorf("Git content filter %q declared in %s is not allowed in source", driver, entryPath)
		}
	}
	return nil
}

// rejectNestedRepositories walks the current source for a repository marker
// below the root. A nested repository is usually untracked, so it would not
// appear in a HEAD checkout at all; refusing it here keeps the source
// unambiguous instead of silently projecting a subset of it later.
func rejectNestedRepositories(sourceRoot string) error {
	return filepath.WalkDir(sourceRoot, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("scan source for nested repositories: %w", err)
		}
		rel, relErr := filepath.Rel(sourceRoot, current)
		if relErr != nil {
			return fmt.Errorf("scan source for nested repositories: %w", relErr)
		}
		if rel == "." {
			return nil
		}
		name := entry.Name()
		if strings.EqualFold(name, gitMarker) {
			if filepath.Dir(rel) == "." {
				// This repository's own storage. A directory is not descended
				// into, but a worktree checkout's marker is a file, and
				// fs.SkipDir on a file skips every remaining entry of the
				// containing directory: here that would abandon the whole scan
				// at the source root.
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return fmt.Errorf("nested repository or submodule at %s is not allowed in source", filepath.ToSlash(filepath.Dir(rel)))
		}
		if entry.IsDir() && isSkipped(name) {
			return fs.SkipDir
		}
		return nil
	})
}

func isSkipped(name string) bool {
	for _, skip := range skipDirs {
		if strings.EqualFold(name, skip) {
			return true
		}
	}
	return false
}

// runGit executes one Git command with separate arguments and returns its
// combined output. Hooks are disabled and terminal prompting is off: creating a
// baseline reads history, it never runs repository-supplied code and never
// waits for input.
func runGit(sourceRoot string, args ...string) (string, error) {
	full := append([]string{"-C", sourceRoot, "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
