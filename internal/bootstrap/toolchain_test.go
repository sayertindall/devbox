package bootstrap

import (
	"strings"
	"testing"

	"devbox/internal/config"
)

// pinnedTools is written out here rather than read from the package: a tool
// dropped from the built-in list must fail this test, which it cannot do if the
// expectation and the implementation are the same value.
func TestDefaultToolsPinsEveryTool(t *testing.T) {
	want := []string{
		"1password-cli",
		"ast-grep",
		"atuin",
		"bun",
		"chezmoi",
		"conftest",
		"fd",
		"herdr",
		"ripgrep",
		"starship",
		"zellij",
		"cosign",
		"cue",
		"dagger",
		"flux2",
		"gh",
		"go",
		"helm",
		"hunk",
		"jq",
		"just",
		"kubectl",
		"kubeconform",
		"node",
		"opentofu",
		"oras",
		"pnpm",
		"python",
		"rust",
		"talosctl",
		"uv",
	}
	tools := DefaultTools()
	if len(tools) != len(want) {
		t.Errorf("the pinned toolchain has %d tools, want %d", len(tools), len(want))
	}
	expected := make(map[string]bool, len(want))
	for _, name := range want {
		expected[name] = true
	}
	for _, name := range want {
		version, ok := tools[name]
		if !ok {
			t.Errorf("the pinned toolchain does not pin %s", name)
			continue
		}
		if version == "" {
			t.Errorf("the pin for %s is empty", name)
			continue
		}
		// An exact version starts with a digit. A floating word such as stable
		// would make two boxes built a week apart disagree.
		if version[0] < '0' || version[0] > '9' {
			t.Errorf("the pin for %s is %q, which is not an exact version", name, version)
		}
	}
	for _, name := range config.SortedTools(tools) {
		if !expected[name] {
			t.Errorf("the pinned toolchain pins %s, which this list does not name", name)
		}
	}
}

// TestDefaultToolsSatisfyTheConfigurationContract pins the built-in list to the
// configuration layer: the boxes would install a toolchain the operator could
// not write into config.toml if the two disagreed.
func TestDefaultToolsSatisfyTheConfigurationContract(t *testing.T) {
	cfg := config.Default()
	cfg.Project = "example-project"
	cfg.ServiceAccount = "devbox@example-project.iam.gserviceaccount.com"
	cfg.RemoteUser = "operator"
	cfg.Tools = DefaultTools()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the pinned toolchain is not a valid configuration: %v", err)
	}
}

// TestMiseUseCommandsAreSortedCanonicalLines guards the one rule the renderer and
// `devbox tools apply` share: the same text, in the same order, so a new box and
// a converged box cannot install different versions.
func TestMiseUseCommandsAreSortedCanonicalLines(t *testing.T) {
	tools := map[string]string{"uv": "0.8.9", "go": "1.26.5", "node": "24.18.0"}
	want := []string{"mise use -g go@1.26.5", "mise use -g node@24.18.0", "mise use -g uv@0.8.9"}
	got := MiseUseCommands(tools)
	if len(got) != len(want) {
		t.Fatalf("MiseUseCommands returned %d commands, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("command %d is %q, want %q", index, got[index], want[index])
		}
	}
	cfg := config.Default()
	cfg.Tools = tools
	script, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, command := range want {
		if !strings.Contains(script, "run_as_login "+command+"\n") {
			t.Errorf("the startup script does not run %q", command)
		}
	}
}
