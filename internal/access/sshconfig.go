package access

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"devbox/internal/box"
	"devbox/internal/config"
)

// sshConfigPath is the operator's SSH configuration, the one file ssh, scp,
// rsync, Zed, and VS Code all read to resolve the devbox alias of a box.
func sshConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

// sshBlock is the result of writing one box's Host entry.
type sshBlock struct {
	// Path is the SSH configuration devbox wrote.
	Path string
	// Lines is the Host entry for this box, as it now reads in the file.
	Lines []string
	// Changed is false when the file already held exactly this entry.
	Changed bool
}

// stanza is one Host entry inside the managed block. Every line is kept verbatim,
// so an entry this run did not write survives a rewrite untouched.
type stanza struct {
	// aliases is the Host line's argument list joined by spaces.
	aliases string
	lines   []string
}

// owns reports whether this entry answers to one alias.
func (s stanza) owns(alias string) bool {
	for _, candidate := range strings.Fields(s.aliases) {
		if candidate == alias {
			return true
		}
	}
	return false
}

// managedRegion is an SSH configuration split into the text before the devbox
// block, the Host entries inside it, and the text after it.
type managedRegion struct {
	before  string
	stanzas []stanza
	after   string
}

// render puts the file back together. The text outside the markers is byte for
// byte what the operator wrote. Inside them the entries are written in alias
// order, so writing another box cannot reshuffle the block and a second run of
// the same command produces an identical file.
func (r managedRegion) render() string {
	var out strings.Builder
	out.WriteString(r.before)
	out.WriteString(box.SSHConfigMarkerStart + "\n")
	for _, entry := range r.stanzas {
		for _, line := range entry.lines {
			out.WriteString(line + "\n")
		}
	}
	out.WriteString(box.SSHConfigMarkerEnd + "\n")
	out.WriteString(r.after)
	return out.String()
}

// writeSSHBlock inserts or replaces the managed Host entry for one box and
// returns what the file now holds for it.
//
// externalIP is the box's address when it has one. An empty value means the box
// is reached through an IAP tunnel, so the entry resolves the instance name and
// sends every connection through gcloud.
func writeSSHBlock(path string, cfg config.Config, name box.Name, externalIP string) (sshBlock, error) {
	current, err := readIfPresent(path)
	if err != nil {
		return sshBlock{}, err
	}
	region, err := splitManaged(current)
	if err != nil {
		return sshBlock{}, fmt.Errorf("%s: %w", path, err)
	}
	fresh := hostStanza(cfg, name, externalIP)
	region.stanzas = mergeStanza(region.stanzas, cfg.SSHHost(name.String()), fresh)
	next := region.render()
	block := sshBlock{Path: path, Lines: fresh.lines, Changed: next != current}
	if block.Changed {
		if err := writeSSHConfig(path, next); err != nil {
			return sshBlock{}, err
		}
		return block, nil
	}
	// The content is already right, the mode still has to be: an SSH
	// configuration that names an identity file should not be world readable.
	if err := os.Chmod(path, 0o600); err != nil {
		return sshBlock{}, fmt.Errorf("set the mode of %s: %w", path, err)
	}
	return block, nil
}

// readIfPresent reads the SSH configuration, returning empty text when the
// operator has none yet.
func readIfPresent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return string(data), nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return "", fmt.Errorf("read %s: %w", path, err)
}

// sshLine is one line of the SSH configuration: its byte range in the file and
// its text without the terminator.
type sshLine struct {
	start int
	end   int
	text  string
}

// scanSSHLines splits the file into lines. A carriage return before the newline
// stays out of the text so markers match either line ending, but it stays in the
// file, so a line outside the managed block is still preserved byte for byte.
func scanSSHLines(raw string) []sshLine {
	var lines []sshLine
	start := 0
	for index := 0; index < len(raw); index++ {
		if raw[index] != '\n' {
			continue
		}
		end := index
		if end > start && raw[end-1] == '\r' {
			end--
		}
		lines = append(lines, sshLine{start: start, end: end, text: raw[start:end]})
		start = index + 1
	}
	if start < len(raw) {
		lines = append(lines, sshLine{start: start, end: len(raw), text: raw[start:]})
	}
	return lines
}

// splitManaged finds the devbox block and splits the file around it.
func splitManaged(raw string) (managedRegion, error) {
	lines := scanSSHLines(raw)
	var starts, ends []int
	for index, line := range lines {
		switch line.text {
		case box.SSHConfigMarkerStart:
			starts = append(starts, index)
		case box.SSHConfigMarkerEnd:
			ends = append(ends, index)
		}
	}
	if len(starts) > 1 || len(ends) > 1 {
		return managedRegion{}, errors.New("the block markers appear more than once; refusing to rewrite a file devbox does not understand")
	}
	if len(starts) != len(ends) {
		return managedRegion{}, fmt.Errorf("the block is truncated: %d start markers and %d end markers", len(starts), len(ends))
	}
	if len(starts) == 0 {
		before := raw
		if before != "" && !strings.HasSuffix(before, "\n") {
			// The block has to start on a line of its own. This is the only byte
			// devbox adds outside the markers, and only to a file that does not
			// end in a newline.
			before += "\n"
		}
		return managedRegion{before: before}, nil
	}
	start, end := starts[0], ends[0]
	if start > end {
		return managedRegion{}, errors.New("the block end marker precedes its start marker")
	}
	entries, err := parseStanzas(lines[start+1 : end])
	if err != nil {
		return managedRegion{}, err
	}
	return managedRegion{
		before:  raw[:lines[start].start],
		stanzas: entries,
		after:   trimOneNewline(raw[lines[end].end:]),
	}, nil
}

// trimOneNewline drops the terminator of the end marker line, so the tail keeps
// every byte the operator wrote after the block.
func trimOneNewline(tail string) string {
	if strings.HasPrefix(tail, "\r\n") {
		return tail[2:]
	}
	return strings.TrimPrefix(tail, "\n")
}

// parseStanzas reads the Host entries inside the managed block. Anything that is
// neither a Host line nor one of its indented directives is an error: devbox
// refuses to rewrite text it would otherwise drop.
func parseStanzas(lines []sshLine) ([]stanza, error) {
	var entries []stanza
	for _, line := range lines {
		if strings.TrimSpace(line.text) == "" {
			continue
		}
		if aliases, ok := hostLine(line.text); ok {
			entries = append(entries, stanza{aliases: aliases, lines: []string{line.text}})
			continue
		}
		if len(entries) == 0 || line.text == strings.TrimLeft(line.text, " \t") {
			return nil, fmt.Errorf("inside the managed block, %q is neither a Host entry nor one of its directives", strings.TrimSpace(line.text))
		}
		last := &entries[len(entries)-1]
		last.lines = append(last.lines, line.text)
	}
	return entries, nil
}

// hostLine reports whether a line opens a Host entry and returns its aliases.
func hostLine(text string) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "host") {
		return "", false
	}
	return strings.Join(fields[1:], " "), true
}

// mergeStanza replaces the entry answering to alias with a fresh one and keeps
// every other entry in alias order.
func mergeStanza(entries []stanza, alias string, fresh stanza) []stanza {
	kept := make([]stanza, 0, len(entries)+1)
	for _, entry := range entries {
		if entry.owns(alias) {
			continue
		}
		kept = append(kept, entry)
	}
	kept = append(kept, fresh)
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].aliases < kept[j].aliases })
	return kept
}

// hostStanza renders the devbox entry for one box.
func hostStanza(cfg config.Config, name box.Name, externalIP string) stanza {
	alias := cfg.SSHHost(name.String())
	host := name.String()
	if externalIP != "" {
		host = externalIP
	}
	lines := []string{"Host " + alias, "  HostName " + host}
	if externalIP == "" {
		lines = append(lines, "  ProxyCommand "+proxyCommand(cfg))
	}
	lines = append(lines, "  User "+cfg.RemoteUser)
	if cfg.SSHKey != "" {
		lines = append(lines, "  IdentityFile "+cfg.SSHKey)
	}
	// accept-new records the box's host key on the first connection. Without it
	// the non-interactive runs below fail on an unknown key, and this box is one
	// devbox created in the operator's own project.
	lines = append(lines, "  StrictHostKeyChecking accept-new")
	return stanza{aliases: alias, lines: append(lines, "  ServerAliveInterval 30")}
}

// proxyCommand reaches a box that has no external address by asking gcloud to
// carry the ssh connection over an IAP tunnel. ssh substitutes %h and %p with the
// entry's HostName and port, so the tunnel resolves the instance name devbox put
// there.
func proxyCommand(cfg config.Config) string {
	return "gcloud compute start-iap-tunnel %h %p --listen-on-stdin " +
		cfg.ProjectFlag() + " " + cfg.ZoneFlag() + " --verbosity=warning"
}

// writeSSHConfig replaces the file atomically, so an interrupted write cannot
// leave the operator without a working SSH configuration, and keeps it readable by
// its owner alone.
func writeSSHConfig(path, content string) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		// A dotfiles repository usually links ~/.ssh/config to the file it
		// manages. Writing the resolved file keeps that link in place.
		target = resolved
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".devbox-ssh-config-")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	name := temp.Name()
	if _, err := temp.WriteString(content); err != nil {
		temp.Close()
		os.Remove(name)
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		os.Remove(name)
		return fmt.Errorf("set the mode of %s: %w", name, err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, target); err != nil {
		os.Remove(name)
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}
