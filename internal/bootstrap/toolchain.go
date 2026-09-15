package bootstrap

import "devbox/internal/config"

// DefaultTools is the pinned toolchain every box installs when the operator's
// configuration declares no [tools] table of its own.
//
// Every version is exact. A floating version would make two boxes built a week
// apart disagree, and the point of the box is that a tree that works on one
// works on the other. The set is resolved by mise; the harness is installed by
// npm and is deliberately not part of this table.
func DefaultTools() map[string]string {
	return map[string]string{
		"conftest":    "0.70.0",
		"cosign":      "3.1.3",
		"cue":         "0.17.1",
		"dagger":      "0.21.9",
		"flux2":       "2.9.5",
		"gh":          "2.100.0",
		"go":          "1.26.5",
		"helm":        "3.16.3",
		"hunk":        "0.19.0",
		"jq":          "1.8.2",
		"just":        "1.42.4",
		"kubectl":     "1.34.11",
		"kubeconform": "0.8.0",
		"node":        "24.18.0",
		"opentofu":    "1.12.6",
		"oras":        "1.3.4",
		"pnpm":        "12.4.1",
		"python":      "3.12.14",
		"rust":        "1.98.0",
		"talosctl":    "1.14.0",
		"uv":          "0.8.9",
	}
}

// DefaultHarnessBinary is the executable the harness package installs. npm puts
// it in the node prefix, and the bootstrap links it into /usr/local/bin so a
// non-interactive ssh command finds it without reading a shell profile.
const DefaultHarnessBinary = "omp"

// MiseUseCommands returns one `mise use -g` command per tool, in sorted tool
// order.
//
// The startup script runs these as the login user, and `devbox tools apply` runs
// the same commands on a running box, so a box built by the bootstrap and a box
// converged later cannot install different versions.
func MiseUseCommands(tools map[string]string) []string {
	names := config.SortedTools(tools)
	commands := make([]string, 0, len(names))
	for _, name := range names {
		commands = append(commands, "mise use -g "+name+"@"+tools[name])
	}
	return commands
}
