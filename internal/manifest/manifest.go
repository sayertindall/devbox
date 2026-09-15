// Package manifest builds the positive source allowlist that defines exactly
// what may activate on the server (REQUIREMENTS FR-003, FR-005, NFR-SEC-003,
// INV-003). A manifest is the complete projection: nothing outside it is ever
// transferred, materialized, or returned.
//
// Containment strategy: every filesystem operation goes through os.Root, so no
// path, symlink, or race can reach outside the source root or the destination
// root. The walk uses Root.Lstat and only descends into real directories, so a
// symlink is recorded as a symlink and never followed; each descent additionally
// requires os.SameFile against the observed directory, so a directory replaced
// mid-walk is an error rather than a redirect. Path and symlink rules are
// enforced in Validate, not only during the walk, so a manifest that arrives
// from elsewhere (staging, the server, a return projection) is checked exactly
// like a locally built one.
//
// os.Root does not validate symlink targets: Root.Symlink will happily create an
// absolute or escaping link, and lexical cleanup of a raw target is not enough
// because a symlink component makes ".." mean the parent of the target, not the
// parent of the link. Target safety is therefore decided by resolving the target
// through the manifest's own entry graph, one raw component at a time.
package manifest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

// Version is the manifest schema version. It is part of the canonical digest.
const Version = 1

// Entry kinds. Directories are implied by entry paths.
//
// ponytail: no directory entries; an empty directory carries no source and Git
// does not track one either. Add a KindDir entry if a project ever needs one.
const (
	KindFile    = "file"
	KindSymlink = "symlink"
)

// tempPrefix names the transient file a materialized entry is written to before
// it is verified and atomically renamed into place.
const tempPrefix = ".agentbox-tmp-"

// gitMarker is a repository or submodule marker: a directory in an ordinary
// checkout, a file in a worktree checkout or a submodule.
const gitMarker = ".git"

// envPrefix matches the ".env.*" family. Both constants are lowercase because
// every comparison against them lowercases the source component first.
const envPrefix = ".env."

// Policy selects the source policy. Only the fixed mandatory allowlist exists
// today, matching config.Project.SourcePolicy == "allowlist".
//
// ponytail: no fields yet. Add per-project excludes here when a project needs
// more than the mandatory list; Materialize must then be given the same policy
// so its undeclared-path rewalk uses identical rules.
type Policy struct{}

// mandatoryExcludes never enter a source or baseline projection. Matching is by
// base name at any depth, so a nested node_modules or sub/.env is excluded too.
var mandatoryExcludes = []string{
	".env",
	".ssh",
	".aws",
	".config",
	".claude",
	".codex",
	".omp",
	".agentbox",
	".git",
	"node_modules",
	"dist",
	"build",
}

// errIrregular and errReplaced are the two ways the stable-open protocol can
// refuse a source path. They are sentinels rather than messages because the
// build phase and the materialize phase phrase the same refusal differently.
var (
	errIrregular = errors.New("source path is not a regular file")
	errReplaced  = errors.New("source path was replaced")
)

type Entry struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Executable bool   `json:"executable"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Target     string `json:"target"`
}

type Manifest struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
	SHA256  string  `json:"sha256"`
	Bytes   int64   `json:"bytes"`
}

// canonical is the byte form the digest covers. It excludes SHA256 so the
// digest is reproducible from the serialized manifest itself.
type canonical struct {
	Version int     `json:"version"`
	Bytes   int64   `json:"bytes"`
	Entries []Entry `json:"entries"`
}

// Build walks the current source below root without following symlinks and
// returns the canonical positive manifest.
func Build(root string, _ Policy) (Manifest, error) {
	source, err := os.OpenRoot(root)
	if err != nil {
		return Manifest{}, fmt.Errorf("open source root: %w", err)
	}
	defer source.Close()
	return buildFromRoot(source)
}

// buildFromRoot builds a manifest from an already-open source root, so a caller
// that also reads files (Materialize) uses one stable boundary for both.
func buildFromRoot(source *os.Root) (Manifest, error) {
	var entries []Entry
	var total int64
	if err := walk(source, ".", nil, &entries, &total); err != nil {
		return Manifest{}, err
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })

	m := Manifest{Version: Version, Entries: entries, Bytes: total}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	digest, err := digestOf(m)
	if err != nil {
		return Manifest{}, err
	}
	m.SHA256 = digest
	return m, nil
}

// walk lists dir and appends its allowed entries. expect is the Lstat result the
// caller observed for dir, or nil for the root: the opened directory must still
// be that same file, so a directory swapped for a symlink mid-walk fails instead
// of redirecting the walk.
func walk(source *os.Root, dir string, expect os.FileInfo, entries *[]Entry, total *int64) error {
	names, err := readStableDir(source, dir, expect)
	if err != nil {
		return err
	}
	for _, dirent := range names {
		rel, skip, err := admit(dir, dirent.Name())
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		observed, err := source.Lstat(rel)
		if err != nil {
			return fmt.Errorf("lstat %s: %w", rel, err)
		}
		if err := recordEntry(source, rel, observed, entries, total); err != nil {
			return err
		}
	}
	return nil
}

// readStableDir opens dir and returns its entries, requiring the opened
// directory to still be the file expect describes. expect is nil for the root.
// A directory that turned into a symlink, a file, or another inode between the
// caller's Lstat and this open is an error rather than a redirect.
func readStableDir(source *os.Root, dir string, expect os.FileInfo) ([]os.DirEntry, error) {
	handle, err := source.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is no longer a directory", dir)
	}
	if expect != nil && !os.SameFile(expect, info) {
		return nil, fmt.Errorf("directory %s was replaced while it was read", dir)
	}
	names, err := handle.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	return names, nil
}

// admit decides whether the directory entry name below dir may enter the
// projection and returns its root-relative path. skip reports an excluded name.
//
// A .git marker below the root means a nested repository or a submodule.
// FR-003 forbids partially copying one, so this is an error rather than a
// silent skip. At the root the marker is this project's own repository storage
// (directory, or a file for a worktree checkout) and is excluded.
func admit(dir, name string) (rel string, skip bool, err error) {
	if isGitMarker(name) {
		if dir == "." {
			return "", true, nil
		}
		return "", false, fmt.Errorf("nested repository or submodule at %s is not allowed in source", dir)
	}
	if isExcluded(name) {
		return "", true, nil
	}
	if err := validateName(name); err != nil {
		return "", false, fmt.Errorf("entry %q in %q: %w", name, dir, err)
	}
	if dir == "." {
		return name, false, nil
	}
	return dir + "/" + name, false, nil
}

// recordEntry classifies the observed path: a directory is descended into, a
// regular file is hashed and declared, a symlink is declared with its raw
// target, and anything else is refused. Target safety needs the whole entry
// graph, so Validate decides it rather than the walk.
func recordEntry(source *os.Root, rel string, observed os.FileInfo, entries *[]Entry, total *int64) error {
	mode := observed.Mode()
	switch {
	case mode.IsDir():
		return walk(source, rel, observed, entries, total)
	case mode.IsRegular():
		size, digest, err := hashFile(source, rel, observed)
		if err != nil {
			return err
		}
		*entries = append(*entries, Entry{
			Path:       rel,
			Kind:       KindFile,
			Executable: mode.Perm()&0o111 != 0,
			Size:       size,
			SHA256:     digest,
		})
		*total += size
		return nil
	case mode&os.ModeSymlink != 0:
		target, err := source.Readlink(rel)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", rel, err)
		}
		*entries = append(*entries, Entry{Path: rel, Kind: KindSymlink, Target: target})
		return nil
	}
	return fmt.Errorf("special file %s is not allowed in source (mode %v)", rel, mode)
}

// requireStableRegular is the stable-open protocol both the hashing path and the
// copying path depend on: the file behind an already-open handle must still be
// the same regular file Lstat reported, so a path swapped for a symlink or
// another inode is refused instead of read. It returns the opened mode and
// errIrregular or errReplaced, so each caller keeps its own phase wording.
func requireStableRegular(handle *os.File, observed os.FileInfo) (os.FileMode, error) {
	opened, err := handle.Stat()
	if err != nil {
		return 0, err
	}
	mode := opened.Mode()
	if !mode.IsRegular() {
		return mode, errIrregular
	}
	if !os.SameFile(observed, opened) {
		return mode, errReplaced
	}
	return mode, nil
}

// hashFile hashes a regular file through the stable-open protocol, so a path
// swapped for a symlink or another inode is an error rather than a read.
func hashFile(source *os.Root, rel string, observed os.FileInfo) (int64, string, error) {
	handle, err := source.Open(rel)
	if err != nil {
		return 0, "", fmt.Errorf("open %s: %w", rel, err)
	}
	defer handle.Close()
	mode, err := requireStableRegular(handle, observed)
	switch {
	case errors.Is(err, errIrregular):
		return 0, "", fmt.Errorf("special file %s is not allowed in source (mode %v)", rel, mode)
	case errors.Is(err, errReplaced):
		return 0, "", fmt.Errorf("source path %s was replaced while it was read", rel)
	case err != nil:
		return 0, "", fmt.Errorf("stat %s: %w", rel, err)
	}
	hasher := sha256.New()
	size, err := io.Copy(hasher, handle)
	if err != nil {
		return 0, "", fmt.Errorf("hash %s: %w", rel, err)
	}
	return size, hex.EncodeToString(hasher.Sum(nil)), nil
}

// Validate enforces every path, kind, and symlink-graph rule on a manifest,
// whether it was built locally or received from elsewhere.
func (m Manifest) Validate() error {
	if m.Version != Version {
		return fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	graph, err := m.index()
	if err != nil {
		return err
	}
	if err := graph.checkNamespace(); err != nil {
		return err
	}
	if err := graph.resolveLinks(m.Entries); err != nil {
		return err
	}
	// Bytes is the staging quota input, so it is authenticated metadata, not a
	// hint: it must be exactly the sum of declared regular-file sizes. This is
	// checked before the digest so a self-consistent forged manifest still fails
	// on the quota rule rather than on its digest.
	if err := checkBytes(m); err != nil {
		return err
	}
	return m.checkDigest()
}

// index validates every entry path, ordering, and kind-specific field, and
// returns the entry graph the symlink rules resolve through.
func (m Manifest) index() (entryGraph, error) {
	graph := entryGraph{
		files:  map[string]bool{},
		links:  map[string]string{},
		dirs:   map[string]bool{},
		claims: map[string]string{},
	}
	previous := ""
	for _, entry := range m.Entries {
		if err := validatePath(entry.Path); err != nil {
			return entryGraph{}, fmt.Errorf("entry %q: %w", entry.Path, err)
		}
		if err := checkOrder(previous, entry.Path); err != nil {
			return entryGraph{}, err
		}
		previous = entry.Path
		if err := graph.claimTree(entry.Path); err != nil {
			return entryGraph{}, err
		}
		if err := graph.add(entry); err != nil {
			return entryGraph{}, err
		}
	}
	return graph, nil
}

// checkOrder requires entry paths to be unique and canonically ascending, so
// the serialization a digest covers is the only valid one for a given set.
func checkOrder(previous, current string) error {
	if previous == "" {
		return nil
	}
	if current == previous {
		return fmt.Errorf("duplicate entry %q", current)
	}
	if current < previous {
		return fmt.Errorf("entries are not canonically ordered at %q", current)
	}
	return nil
}

// checkFileFields enforces the fields a regular-file entry must and must not
// declare.
func checkFileFields(entry Entry) error {
	if entry.Target != "" {
		return fmt.Errorf("entry %q: file may not declare a symlink target", entry.Path)
	}
	if entry.Size < 0 {
		return fmt.Errorf("entry %q: negative size", entry.Path)
	}
	if len(entry.SHA256) != 64 {
		return fmt.Errorf("entry %q: malformed content digest", entry.Path)
	}
	if _, err := hex.DecodeString(entry.SHA256); err != nil {
		return fmt.Errorf("entry %q: malformed content digest", entry.Path)
	}
	return nil
}

// checkSymlinkFields enforces the fields a symlink entry must and must not
// declare. Target safety itself is decided by resolution through the graph.
func checkSymlinkFields(entry Entry) error {
	if entry.Size != 0 || entry.SHA256 != "" {
		return fmt.Errorf("entry %q: symlink may not declare size or digest", entry.Path)
	}
	if entry.Target == "" {
		return fmt.Errorf("entry %q: symlink target is empty", entry.Path)
	}
	return nil
}

// entryGraph is the manifest's own view of the tree: declared files, declared
// symlinks, the directories those entry paths imply, and the case-insensitive
// path space every one of them claims.
type entryGraph struct {
	files  map[string]bool
	links  map[string]string
	dirs   map[string]bool
	claims map[string]string
}

// add validates an entry's kind-specific fields and records it in the graph.
func (g entryGraph) add(entry Entry) error {
	switch entry.Kind {
	case KindFile:
		if err := checkFileFields(entry); err != nil {
			return err
		}
		g.files[entry.Path] = true
	case KindSymlink:
		if err := checkSymlinkFields(entry); err != nil {
			return err
		}
		g.links[entry.Path] = entry.Target
	default:
		return fmt.Errorf("entry %q: unsupported kind %q", entry.Path, entry.Kind)
	}
	return nil
}

// claim records one spelling of a path in the case-insensitive path space. Two
// different spellings of the same key would resolve to one file on APFS or
// NTFS, so the second one is refused.
func (g entryGraph) claim(p string) error {
	key := strings.ToLower(p)
	if other, ok := g.claims[key]; ok {
		if other != p {
			return fmt.Errorf("path case collision between %q and %q", other, p)
		}
		return nil
	}
	g.claims[key] = p
	return nil
}

// claimTree claims an entry path and every directory that path implies, and
// records those directories in the graph.
func (g entryGraph) claimTree(p string) error {
	if err := g.claim(p); err != nil {
		return err
	}
	// path.Dir is a fixed point at "/" and "..", so terminate on those too
	// rather than relying on validatePath having already rejected them.
	for dir := path.Dir(p); dir != "." && dir != "/" && dir != ".."; dir = path.Dir(dir) {
		if err := g.claim(dir); err != nil {
			return err
		}
		g.dirs[dir] = true
	}
	return nil
}

// checkNamespace rejects an entry that is simultaneously a leaf and a directory
// prefix of another entry, which no filesystem could represent.
func (g entryGraph) checkNamespace() error {
	for file := range g.files {
		if g.dirs[file] {
			return fmt.Errorf("entry %q is declared as both a file and a directory prefix", file)
		}
	}
	for link := range g.links {
		if g.dirs[link] {
			return fmt.Errorf("entry %q is declared as both a symlink and a directory prefix", link)
		}
	}
	return nil
}

// resolveLinks requires every symlink to resolve through the entry graph to
// something declared inside the root. This is what makes an internal symlink
// safe to preserve.
func (g entryGraph) resolveLinks(entries []Entry) error {
	for _, entry := range entries {
		if entry.Kind != KindSymlink {
			continue
		}
		if _, _, err := g.resolve(entry.Path, nil); err != nil {
			return fmt.Errorf("entry %q: %w", entry.Path, err)
		}
	}
	return nil
}

// checkDigest verifies a declared canonical digest. An empty digest means the
// manifest is still being built and has none to verify yet.
func (m Manifest) checkDigest() error {
	if m.SHA256 == "" {
		return nil
	}
	digest, err := digestOf(m)
	if err != nil {
		return err
	}
	if digest != m.SHA256 {
		return fmt.Errorf("manifest digest mismatch: declared %s, computed %s", m.SHA256, digest)
	}
	return nil
}

// parseTarget validates the raw symlink target declared at linkPath and returns
// it with its raw components, before any resolution starts.
func (g entryGraph) parseTarget(linkPath string) (string, []string, error) {
	target := g.links[linkPath]
	if target == "" {
		return "", nil, errors.New("symlink target is empty")
	}
	if strings.HasPrefix(target, "/") {
		return "", nil, errors.New("absolute symlink target is not allowed")
	}
	if !utf8.ValidString(target) {
		return "", nil, errors.New("symlink target is not valid UTF-8")
	}
	if strings.ContainsAny(target, "\\\n\r\x00") {
		return "", nil, errors.New("invalid symlink target")
	}
	components := strings.Split(target, "/")
	if err := checkExcludedComponents(target, components); err != nil {
		return "", nil, err
	}
	return target, components, nil
}

// checkExcludedComponents decides exclusion over every raw component before
// resolution starts, so the verdict cannot depend on resolution order or on
// whether an intermediate directory happens to be declared. ".git" is in the
// mandatory list, so this covers repository and submodule markers too.
func checkExcludedComponents(target string, components []string) error {
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			continue
		}
		if isExcluded(component) {
			return fmt.Errorf("symlink target %q reaches excluded path %q", target, component)
		}
	}
	return nil
}

// resolve resolves the symlink at linkPath through the graph, one raw target
// component at a time, so a ".." after a symlink component means the parent of
// that symlink's target rather than the parent of the link. It returns the
// resolved root-relative path, which is "." for the root itself.
func (g entryGraph) resolve(linkPath string, stack []string) (string, bool, error) {
	if slices.Contains(stack, linkPath) {
		return "", false, fmt.Errorf("symlink cycle through %q", linkPath)
	}
	target, components, err := g.parseTarget(linkPath)
	if err != nil {
		return "", false, err
	}
	stack = append(stack, linkPath)

	current := path.Dir(linkPath)
	if current == "" {
		current = "."
	}
	// Raw components are walked without pre-filtering, because a component after
	// a regular file is ENOTDIR on a real filesystem even when it is "" or "."
	// (a trailing slash, "file/." or "file/..").
	currentIsFile := false
	for _, component := range components {
		if currentIsFile {
			return "", false, fmt.Errorf("symlink target %q cannot traverse through declared file %q", target, current)
		}
		switch component {
		case "", ".":
			continue
		case "..":
			if current == "." {
				return "", false, fmt.Errorf("symlink target %q resolves outside root", target)
			}
			current = path.Dir(current)
			continue
		}
		current, currentIsFile, err = g.step(current, component, target, stack)
		if err != nil {
			return "", false, err
		}
	}
	return g.settle(current, currentIsFile, target)
}

// step advances the resolution cursor from current through one ordinary raw
// component, reporting whether the new cursor is a declared regular file.
func (g entryGraph) step(current, component, target string, stack []string) (string, bool, error) {
	if err := validateName(component); err != nil {
		return "", false, fmt.Errorf("symlink target %q: %w", target, err)
	}
	next := component
	if current != "." {
		next = current + "/" + component
	}
	switch {
	case g.links[next] != "":
		return g.resolve(next, stack)
	case g.files[next]:
		return next, true, nil
	case g.dirs[next]:
		return next, false, nil
	}
	return "", false, fmt.Errorf("symlink target %q is dangling: %q is not a declared entry", target, next)
}

// settle reports the resolved cursor once every raw component is consumed. A
// cursor that is neither the root, a declared directory, nor a declared file is
// a dangling target.
func (g entryGraph) settle(current string, isFile bool, target string) (string, bool, error) {
	if isFile {
		return current, true, nil
	}
	if current == "." || g.dirs[current] {
		return current, false, nil
	}
	return "", false, fmt.Errorf("symlink target %q is dangling", target)
}

// Compare reports the first difference between a declared manifest and an
// actual rewalk of a tree. It is the manifest-authority check that rejects
// extra, missing, or mismatched paths.
//
// Both sides are validated before anything is compared: comparing two
// projections is only meaningful once each one is known well-formed on its own,
// otherwise two identically malformed manifests would compare equal.
func Compare(expected, actual Manifest) error {
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("declared manifest: %w", err)
	}
	if err := actual.Validate(); err != nil {
		return fmt.Errorf("actual manifest: %w", err)
	}
	return compareProjections(expected, actual)
}

// compareProjections compares two manifests that the caller has already
// validated. Only Compare and Materialize may call it, and both validate first.
func compareProjections(expected, actual Manifest) error {
	declared := make(map[string]Entry, len(expected.Entries))
	for _, entry := range expected.Entries {
		declared[entry.Path] = entry
	}
	present := make(map[string]bool, len(actual.Entries))
	for _, entry := range actual.Entries {
		present[entry.Path] = true
		want, ok := declared[entry.Path]
		if !ok {
			return fmt.Errorf("undeclared source path %q", entry.Path)
		}
		if err := compareEntry(want, entry); err != nil {
			return err
		}
	}
	for _, entry := range expected.Entries {
		if !present[entry.Path] {
			return fmt.Errorf("missing declared path %q", entry.Path)
		}
	}
	if expected.Bytes != actual.Bytes {
		return fmt.Errorf("declared byte total %d, found %d", expected.Bytes, actual.Bytes)
	}
	return nil
}

// compareEntry reports the first field of a declared entry the observed entry
// at the same path does not match.
func compareEntry(want, found Entry) error {
	switch {
	case want.Kind != found.Kind:
		return fmt.Errorf("path %q: declared kind %q, found %q", found.Path, want.Kind, found.Kind)
	case want.Executable != found.Executable:
		return fmt.Errorf("path %q: executable bit differs", found.Path)
	case want.Size != found.Size:
		return fmt.Errorf("path %q: declared size %d, found %d", found.Path, want.Size, found.Size)
	case want.SHA256 != found.SHA256:
		return fmt.Errorf("path %q: content digest differs", found.Path)
	case want.Target != found.Target:
		return fmt.Errorf("path %q: declared symlink target %q, found %q", found.Path, want.Target, found.Target)
	}
	return nil
}

// Materialize copies exactly the declared entries from sourceRoot into the
// caller's trusted destination root.
//
// The destination is a pre-opened *os.Root, never a path string: only the caller
// can prove which directory it approved, and a path would be re-resolved here
// through ancestors an attacker may have replaced with symlinks in the meantime.
// Materialize creates and opens no destination path of its own.
//
// Each declared path appears in the destination only after its bytes are
// verified: content is written to a temporary file in the same destination
// directory, hashed while copying, compared against the declared size and
// digest, and only then atomically renamed into place. A failure removes the
// temporary file, so a rejected entry never leaves unverified bytes behind.
func Materialize(sourceRoot string, m Manifest, destination *os.Root) error {
	if destination == nil {
		return errors.New("destination root is required")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	source, err := os.OpenRoot(sourceRoot)
	if err != nil {
		return fmt.Errorf("open source root: %w", err)
	}
	defer source.Close()

	// Manifest authority: rewalk through the same source root the copies use, so
	// no extra, missing, or mismatched path can enter the destination.
	actual, err := buildFromRoot(source)
	if err != nil {
		return err
	}
	// Both sides are already validated here, m just above and actual inside
	// buildFromRoot, so the projection comparison needs no third validation.
	if err := compareProjections(m, actual); err != nil {
		return err
	}

	for _, entry := range m.Entries {
		if err := materializeEntry(source, destination, entry); err != nil {
			return err
		}
	}
	return nil
}

// materializeEntry lands one declared entry, creating the destination
// directories its path implies first.
func materializeEntry(source, destination *os.Root, entry Entry) error {
	if dir := path.Dir(entry.Path); dir != "." {
		if err := destination.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	switch entry.Kind {
	case KindFile:
		return copyFile(source, destination, entry)
	case KindSymlink:
		return copySymlink(destination, entry)
	}
	return fmt.Errorf("entry %q: unsupported kind %q", entry.Path, entry.Kind)
}

func copyFile(source, destination *os.Root, entry Entry) error {
	in, err := openStableSource(source, entry.Path)
	if err != nil {
		return err
	}
	defer in.Close()

	mode := os.FileMode(0o644)
	if entry.Executable {
		mode = 0o755
	}
	temp, out, err := createTemp(destination, entry.Path, mode)
	if err != nil {
		return fmt.Errorf("materialize %s: %w", entry.Path, err)
	}
	committed := false
	defer func() {
		if !committed {
			destination.Remove(temp)
		}
	}()

	size, digest, err := copyAndHash(out, in)
	if err != nil {
		return fmt.Errorf("materialize %s: %w", entry.Path, err)
	}
	if size != entry.Size || digest != entry.SHA256 {
		return fmt.Errorf("materialize %s: source content changed after the manifest was built", entry.Path)
	}
	if err := commitTemp(destination, temp, entry.Path, mode); err != nil {
		return err
	}
	committed = true
	return nil
}

// openStableSource opens a declared source file for copying through the same
// stable-open protocol the hashing path uses, so a path replaced between the
// manifest build and this read is refused instead of copied.
func openStableSource(source *os.Root, rel string) (*os.File, error) {
	observed, err := source.Lstat(rel)
	if err != nil {
		return nil, fmt.Errorf("materialize %s: %w", rel, err)
	}
	if !observed.Mode().IsRegular() {
		return nil, fmt.Errorf("materialize %s: source is no longer a regular file (mode %v)", rel, observed.Mode())
	}
	in, err := source.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("materialize %s: %w", rel, err)
	}
	if _, err := requireStableRegular(in, observed); err != nil {
		in.Close()
		if errors.Is(err, errIrregular) || errors.Is(err, errReplaced) {
			return nil, fmt.Errorf("materialize %s: source was replaced after the manifest was built", rel)
		}
		return nil, fmt.Errorf("materialize %s: %w", rel, err)
	}
	return in, nil
}

// copyAndHash streams in into out while hashing, and returns the bytes written
// and their digest. out is closed either way, and a copy failure is reported
// ahead of a close failure because it is the more specific cause.
func copyAndHash(out *os.File, in io.Reader) (int64, string, error) {
	hasher := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(out, hasher), in)
	closeErr := out.Close()
	if copyErr != nil {
		return 0, "", copyErr
	}
	if closeErr != nil {
		return 0, "", closeErr
	}
	return size, hex.EncodeToString(hasher.Sum(nil)), nil
}

// commitTemp publishes a fully verified temporary file at its declared path.
func commitTemp(destination *os.Root, temp, target string, mode os.FileMode) error {
	if err := destination.Chmod(temp, mode); err != nil {
		return fmt.Errorf("materialize %s: %w", target, err)
	}
	if err := destination.Rename(temp, target); err != nil {
		return fmt.Errorf("materialize %s: %w", target, err)
	}
	return nil
}

// copySymlink creates a validated internal symlink the same way a file lands:
// through a temporary name and an atomic rename.
func copySymlink(destination *os.Root, entry Entry) error {
	temp := tempName(entry.Path)
	if err := destination.Symlink(entry.Target, temp); err != nil {
		return fmt.Errorf("materialize symlink %s: %w", entry.Path, err)
	}
	if err := destination.Rename(temp, entry.Path); err != nil {
		destination.Remove(temp)
		return fmt.Errorf("materialize symlink %s: %w", entry.Path, err)
	}
	return nil
}

// createTemp opens a fresh temporary file next to target inside destination, so
// the later rename stays within one directory and is atomic.
func createTemp(destination *os.Root, target string, mode os.FileMode) (string, *os.File, error) {
	var lastErr error
	for range 8 {
		name := tempName(target)
		handle, err := destination.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err == nil {
			return name, handle, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
		lastErr = err
	}
	return "", nil, fmt.Errorf("create temporary file next to %q: %w", target, lastErr)
}

func tempName(target string) string {
	name := tempPrefix + rand.Text()
	if dir := path.Dir(target); dir != "." {
		return dir + "/" + name
	}
	return name
}

// Encode returns the canonical serialization. Entry order is the manifest order,
// which Validate requires to be ascending by path, so the bytes are stable.
func Encode(m Manifest) ([]byte, error) {
	entries := m.Entries
	if entries == nil {
		entries = []Entry{}
	}
	// JSON escapes invalid UTF-8 into U+FFFD, which would silently change a path
	// and let two distinct paths encode identically, so reject it up front.
	for _, entry := range entries {
		if !utf8.ValidString(entry.Path) {
			return nil, fmt.Errorf("entry %q: path is not valid UTF-8", entry.Path)
		}
		if !utf8.ValidString(entry.Target) {
			return nil, fmt.Errorf("entry %q: symlink target is not valid UTF-8", entry.Path)
		}
	}
	// Serialized quota metadata must never leave here unauthenticated. Validate
	// runs the same check; Encode repeats it because Write and digestOf reach the
	// wire format through this function. It cannot call Validate itself, since
	// Validate computes the digest through Encode.
	if err := checkBytes(m); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(canonical{Version: m.Version, Bytes: m.Bytes, Entries: entries})
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return encoded, nil
}

// checkBytes enforces the staging-quota invariant: Bytes is nonnegative and is
// exactly the sum of every declared regular-file size.
func checkBytes(m Manifest) error {
	if m.Bytes < 0 {
		return fmt.Errorf("manifest byte total is negative: %d", m.Bytes)
	}
	var sum int64
	for _, entry := range m.Entries {
		if entry.Kind != KindFile {
			continue
		}
		if entry.Size < 0 {
			return fmt.Errorf("entry %q: negative size", entry.Path)
		}
		if sum > math.MaxInt64-entry.Size {
			return fmt.Errorf("manifest byte total overflows at entry %q", entry.Path)
		}
		sum += entry.Size
	}
	if m.Bytes != sum {
		return fmt.Errorf("manifest byte total %d does not match the sum of declared entry sizes %d", m.Bytes, sum)
	}
	return nil
}

// Write stores the canonical serialization. The digest is recomputable from it.
func Write(path string, m Manifest) error {
	encoded, err := Encode(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

func digestOf(m Manifest) (string, error) {
	encoded, err := Encode(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// isExcluded reports whether a single path component is mandatorily excluded.
//
// The comparison is case-insensitive because APFS and NTFS resolve ".ENV" and
// ".env" to the same file, so a case variant would otherwise walk straight past
// the allowlist and carry the secret into the projection. Only the comparison is
// normalized: an allowed component keeps its original source spelling in the
// manifest path. Every name in mandatoryExcludes is ASCII, so simple lowercasing
// is sufficient and no Unicode normalization is needed.
func isExcluded(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, envPrefix) {
		return true
	}
	return slices.Contains(mandatoryExcludes, lower)
}

// isGitMarker reports whether a component names repository or submodule
// metadata, case-insensitively for the same reason as isExcluded.
func isGitMarker(name string) bool {
	return strings.ToLower(name) == gitMarker
}

func validateName(name string) error {
	if name == "" || name == "." || name == ".." {
		return errors.New("invalid relative path component")
	}
	if strings.ContainsAny(name, "/\\\n\r\x00") {
		return errors.New("invalid relative path component")
	}
	if !utf8.ValidString(name) {
		return errors.New("path component is not valid UTF-8")
	}
	return nil
}

func validatePath(p string) error {
	if p == "" || p != path.Clean(p) || strings.HasPrefix(p, "/") {
		return errors.New("path is not a normalized relative path")
	}
	for _, component := range strings.Split(p, "/") {
		if err := validateName(component); err != nil {
			return err
		}
		if isExcluded(component) {
			return fmt.Errorf("path component %q is excluded from source", component)
		}
	}
	return nil
}
