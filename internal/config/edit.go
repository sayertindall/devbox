package config

import (
	"fmt"
	"os"
	"strings"
)

// SetValue replaces one top-level setting in the file at path, preserving every
// other byte, and refuses the edit when the result would not decode. It is how
// devbox changes a single key without rewriting the operator's comments.
func SetValue(path, key, value string) error {
	if err := validateToolName(key); err != nil {
		return fmt.Errorf("invalid setting name: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("setting %s needs a value", key)
	}
	if strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("setting %s contains a line break", key)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read configuration %s: %w", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	line := fmt.Sprintf("%s = %s", key, quote(value))
	replaced := false
	insertAtLine := -1
	inTable := false
	for index, current := range lines {
		trimmed := strings.TrimSpace(current)
		if strings.HasPrefix(trimmed, "[") {
			inTable = true
			continue
		}
		if inTable {
			continue
		}
		if existing, ok := toolsKey(current); ok && !inTable && existing == key {
			lines[index] = line
			replaced = true
			break
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			insertAtLine = index + 1
		}
	}
	if !replaced {
		if insertAtLine < 0 {
			insertAtLine = 0
		}
		lines = insertAt(lines, insertAtLine, line)
	}
	candidate := strings.Join(lines, "\n") + "\n"
	if _, err := Decode([]byte(candidate)); err != nil {
		return fmt.Errorf("refusing to write %s: %w", path, err)
	}
	return writeFile(path, []byte(candidate))
}
