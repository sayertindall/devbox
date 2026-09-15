package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSaveLoadRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Project = "example-project"
	cfg.ServiceAccount = "devbox@example-project.iam.gserviceaccount.com"
	cfg.BootstrapURL = "gs://bucket/devbox/startup-script.sh"
	cfg.Tools = map[string]string{"node": "24.18.0", "opentofu": "1.12.6"}
	path := filepath.Join(t.TempDir(), "config.toml")

	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project != cfg.Project || loaded.BootstrapURL != cfg.BootstrapURL || loaded.MachineType != cfg.MachineType {
		t.Fatalf("settings did not survive the round trip: %+v", loaded)
	}
	if loaded.Tools["node"] != "24.18.0" || loaded.Tools["opentofu"] != "1.12.6" {
		t.Fatalf("tools did not survive the round trip: %+v", loaded.Tools)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[tools]") {
		t.Fatal("saved file has no tools table")
	}
	if !strings.Contains(string(data), "devbox tools add") {
		t.Fatal("saved file does not tell the operator how to manage the toolchain")
	}
}

func TestDecodeRejectsUnknownKey(t *testing.T) {
	if _, err := Decode([]byte("project = \"p\"\nnope = 1\n")); err == nil {
		t.Fatal("an unknown key must be rejected so a typo cannot provision the wrong machine")
	}
}

func TestSetToolAddsTableAndKeepsComments(t *testing.T) {
	path := writeTemp(t, "# my settings\nproject = \"p\"\nservice_account = \"sa@example.com\"\n")
	if err := SetTool(path, "node", "24.18.0"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "# my settings") {
		t.Fatalf("hand-written comment was lost:\n%s", text)
	}
	if !strings.Contains(text, "[tools]") || !strings.Contains(text, `node = "24.18.0"`) {
		t.Fatalf("pin was not written:\n%s", text)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tools["node"] != "24.18.0" {
		t.Fatalf("file does not read back the pin: %+v", cfg.Tools)
	}
}

func TestSetToolReplacesOnlyThePinnedLine(t *testing.T) {
	path := writeTemp(t, "project = \"p\"\nservice_account = \"sa@example.com\"\n\n[tools]\n# keep me\nnode = \"1.0.0\"\npython = \"3.12.14\"\n")
	if err := SetTool(path, "node", "24.18.0"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "# keep me") {
		t.Fatalf("comment inside the tools table was lost:\n%s", text)
	}
	if !strings.Contains(text, `node = "24.18.0"`) || strings.Contains(text, `node = "1.0.0"`) {
		t.Fatalf("pin was not replaced:\n%s", text)
	}
	if !strings.Contains(text, `python = "3.12.14"`) {
		t.Fatalf("a sibling pin was lost:\n%s", text)
	}
}

func TestSetToolRefusesAValueThatBreaksTheFile(t *testing.T) {
	path := writeTemp(t, "project = \"p\"\nservice_account = \"sa@example.com\"\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetTool(path, "node", "24.18.0 #oops"); err == nil {
		t.Fatal("a version with a comment character must be refused")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a refused edit still changed the file")
	}
}

func TestRemoveTool(t *testing.T) {
	path := writeTemp(t, "project = \"p\"\nservice_account = \"sa@example.com\"\n\n[tools]\nnode = \"1.0.0\"\npython = \"3.12.14\"\n")
	if err := RemoveTool(path, "node"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := cfg.Tools["node"]; exists {
		t.Fatalf("pin was not removed: %+v", cfg.Tools)
	}
	if cfg.Tools["python"] != "3.12.14" {
		t.Fatalf("a sibling pin was lost: %+v", cfg.Tools)
	}
	if err := RemoveTool(path, "node"); err == nil {
		t.Fatal("removing a pin that is not declared must be an error")
	}
}

func TestSetValueReplacesTopLevelSetting(t *testing.T) {
	path := writeTemp(t, "# harness\nomp_package = \"@oh-my-pi/pi-coding-agent\"\nomp_version = \"1.0.0\"\nproject = \"p\"\nservice_account = \"sa@example.com\"\n")
	if err := SetValue(path, "omp_version", "2.0.0"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OmpVersion != "2.0.0" {
		t.Fatalf("setting was not replaced: %q", cfg.OmpVersion)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# harness") {
		t.Fatalf("comment was lost:\n%s", string(data))
	}
	if strings.Count(string(data), "omp_version") != 1 {
		t.Fatalf("setting appears more than once:\n%s", string(data))
	}
}

func TestEffectiveToolsFallsBackToBuiltins(t *testing.T) {
	builtins := map[string]string{"node": "24.18.0"}
	cfg := Default()
	if got := cfg.EffectiveTools(builtins); got["node"] != "24.18.0" {
		t.Fatalf("an empty table must mean the built-in toolchain: %+v", got)
	}
	cfg.Tools = map[string]string{"python": "3.12.14"}
	got := cfg.EffectiveTools(builtins)
	if _, exists := got["node"]; exists {
		t.Fatalf("declared pins must replace the built-ins entirely: %+v", got)
	}
	if got["python"] != "3.12.14" {
		t.Fatalf("declared pin missing: %+v", got)
	}
}

func TestStatePathStaysInsideTheStateRoot(t *testing.T) {
	t.Setenv("DEVBOX_HOME", t.TempDir())
	path, err := StatePath("trees.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "trees.json") {
		t.Fatalf("unexpected path: %q", path)
	}
	nested, err := StatePath("agents", "dev", "handoff.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(nested, "agents/dev/handoff.json") {
		t.Fatalf("nested state path is wrong: %q", nested)
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := StatePath(bad); err == nil {
			t.Fatalf("state path component %q must be rejected so it cannot escape the state root", bad)
		}
	}
}

func TestInitWritesFromNothingAndReportsTheBlankSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	cfg := Default()
	cfg.Project = "example-project"
	missing, err := Init(path, cfg)
	if err != nil {
		t.Fatalf("init must work on a machine with no state directory yet: %v", err)
	}
	if len(missing) == 0 {
		t.Fatal("init must report the settings that are still blank")
	}
	blank := strings.Join(missing, " ")
	if !strings.Contains(blank, "service_account") || !strings.Contains(blank, "bootstrap_url") {
		t.Fatalf("the report must name what a machine cannot be created without: %s", blank)
	}
	// The written file must be readable, and it must be readable before every
	// setting is filled in: that is the whole point of init.
	if _, err := Parse(mustRead(t, path)); err != nil {
		t.Fatalf("a freshly written file must parse: %v", err)
	}
	if _, err := Decode(mustRead(t, path)); err == nil {
		t.Fatal("an incomplete configuration must still be refused by the strict reader")
	}
	if _, err := Init(path, Config{}); err == nil {
		t.Fatal("init without a project must fail")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
