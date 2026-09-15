package config

import (
	"fmt"
	"os"
	"strings"
)

// toolsTable is the TOML table these functions edit.
const toolsTable = "[tools]"

// SetTool inserts or replaces one entry in the [tools] table of the file at
// path, leaving every other byte alone so hand-written comments survive.
//
// The new text is decoded before it is written: an edit that would produce a
// file devbox cannot read is refused instead of installed.
func SetTool(path, name, version string) error {
	return editTools(path, func(lines []string, block block) ([]string, error) {
		key := toolKey(name)
		line := fmt.Sprintf("%s = %s", key, quote(version))
		for index := block.start; index < block.end; index++ {
			if existing, ok := toolsKey(lines[index]); ok && existing == name {
				lines[index] = line
				return lines, nil
			}
		}
		if block.start < 0 {
			// No table yet: append one at the end of the file.
			lines = append(lines, toolsTable, line)
			return lines, nil
		}
		lines = insertAt(lines, block.end, line)
		return lines, nil
	}, name, version)
}

// RemoveTool deletes one entry from the [tools] table. Removing the last entry
// leaves the empty table in place, which reads back as "no pins declared".
func RemoveTool(path, name string) error {
	return editTools(path, func(lines []string, block block) ([]string, error) {
		if block.start < 0 {
			return nil, fmt.Errorf("no [tools] table in %s", path)
		}
		for index := block.start; index < block.end; index++ {
			if existing, ok := toolsKey(lines[index]); ok && existing == name {
				return append(lines[:index:index], lines[index+1:]...), nil
			}
		}
		return nil, fmt.Errorf("tool %s is not pinned in %s", name, path)
	}, name, "")
}

// LoadTools reads just the pinned table.
func LoadTools(path string) (map[string]string, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	return cfg.Tools, nil
}

// block is the half-open line range of the [tools] table, or -1 when absent.
type block struct{ start, end int }

func findToolsTable(lines []string) block {
	start := -1
	for index, line := range lines {
		if strings.TrimSpace(line) != toolsTable {
			continue
		}
		start = index + 1
		break
	}
	if start < 0 {
		return block{start: -1, end: -1}
	}
	end := len(lines)
	for index := start; index < len(lines); index++ {
		if strings.HasPrefix(strings.TrimSpace(lines[index]), "[") {
			end = index
			break
		}
	}
	return block{start: start, end: end}
}

// toolsKey reports the tool name a table line pins, if the line is an entry.
func toolsKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	key, _, found := strings.Cut(trimmed, "=")
	if !found {
		return "", false
	}
	key = strings.TrimSpace(key)
	if unquoted, err := unquoteKey(key); err == nil {
		key = unquoted
	}
	return key, key != ""
}

func unquoteKey(key string) (string, error) {
	if len(key) >= 2 && strings.HasPrefix(key, `"`) && strings.HasSuffix(key, `"`) {
		return strings.TrimSuffix(strings.TrimPrefix(key, `"`), `"`), nil
	}
	return key, nil
}

func insertAt(lines []string, index int, line string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:index]...)
	out = append(out, line)
	out = append(out, lines[index:]...)
	return out
}

// editTools reads the file, hands its lines to mutate, decodes the result, and
// only then installs it.
func editTools(path string, mutate func([]string, block) ([]string, error), name, version string) error {
	if name != "" {
		if err := validateToolName(name); err != nil {
			return err
		}
	}
	if version != "" {
		if err := validateVersion(version); err != nil {
			return fmt.Errorf("tool %s: %w", name, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read configuration %s: %w", path, err)
	}
	text := string(data)
	if !strings.Contains(text, toolsTable) && strings.TrimSpace(text) == "" {
		return fmt.Errorf("configuration %s is empty; create it with devbox config init", path)
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	block := findToolsTable(lines)
	updated, err := mutate(lines, block)
	if err != nil {
		return err
	}
	candidate := strings.Join(updated, "\n") + "\n"
	cfg, err := Decode([]byte(candidate))
	if err != nil {
		return fmt.Errorf("refusing to write %s: %w", path, err)
	}
	if cfg.Tools == nil {
		cfg.Tools = map[string]string{}
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("refusing to write %s: %w", path, err)
	}
	return writeFile(path, []byte(candidate))
}

// validateToolName rejects a name that cannot be a TOML key or a shell word in
// the convergence command.
func validateToolName(name string) error {
	if name == "" {
		return fmt.Errorf("tool name is required")
	}
	if strings.ContainsAny(name, " \t\r\n\"'#=[]") {
		return fmt.Errorf("tool name %q contains a character devbox cannot use", name)
	}
	return nil
}

// validateVersion rejects a version that would break the file or the shell. A
// floating word such as stable is allowed but discouraged, because two boxes
// built a week apart would then disagree.
func validateVersion(version string) error {
	if strings.TrimSpace(version) == "" {
		return fmt.Errorf("version is required")
	}
	if strings.ContainsAny(version, " \t\r\n\"'#=") {
		return fmt.Errorf("version %q contains a character devbox cannot use", version)
	}
	return nil
}
