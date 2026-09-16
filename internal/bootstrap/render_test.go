package bootstrap

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"devbox/internal/box"
	"devbox/internal/config"
)

// artifactUser is the login user baked into deploy/startup-script.sh.
// config.Default() takes the login user from the environment, so the comparison
// between the artifact and the renderer pins it instead of depending on whoever
// runs the test.
const artifactUser = "sayertindall"

// artifactPath is the checked-in rendering the deploy step publishes.
const artifactPath = "../../deploy/startup-script.sh"

func TestRenderIncludesRequiredLines(t *testing.T) {
	script, err := Render(config.Default())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	required := []struct {
		what string
		want string
	}{
		{"interpreter", "#!/bin/bash"},
		{"strict mode", "set -euo pipefail"},
		{"explicit path", "export PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'"},
		{"bootstrap log", "LOG='" + box.BootstrapLog + "'"},
		{"bootstrap stamp", "STAMP='" + box.BootstrapStamp + "'"},
		{"failure mark", "FAILED='" + box.BootstrapFailed + "'"},
		// The mark is what lets a client tell a failed install from a slow one, so
		// both halves of it are required: the trap that writes it and the clear that
		// keeps a stale one from outliving the install that failed.
		{"failure trap", "trap 'status=$?;"},
		{"failure mark cleared on success", "rm -f \"$FAILED\""},
		{"data mount", "DATA_MOUNT='/mnt/data'"},
		{"data disk device", "DATA_DEVICE='/dev/disk/by-id/google-devbox-data'"},
		{"docker root", "DOCKER_ROOT='" + box.DockerRoot + "'"},
		{"containerd root", "CONTAINERD_ROOT='" + box.ContainerdRoot + "'"},
		{"docker daemon data root", `"data-root": "` + box.DockerRoot + `"`},
		{"docker daemon file", "/etc/docker/daemon.json"},
		{"containerd schema", "version = 2"},
		{"containerd root setting", `root = "` + box.ContainerdRoot + `"`},
		{"containerd config file", "/etc/containerd/config.toml"},
		{"containerd restart", "systemctl restart containerd"},
		{"docker restart", "systemctl restart docker"},
		{"data disk filesystem check", `if ! blkid "$DATA_DEVICE" >/dev/null 2>&1; then`},
		{"data disk format", `mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0 "$DATA_DEVICE"`},
		{"mount by uuid", `grep -qs "UUID=$DATA_UUID" /etc/fstab`},
		{"fstab entry", `printf 'UUID=%s %s ext4 defaults,nofail,discard 0 2\n' "$DATA_UUID" "$DATA_MOUNT" >> /etc/fstab`},
		{"mountpoint", `mountpoint -q "$DATA_MOUNT"`},
		{"data directories", `install -d -m 0755 "$DOCKER_ROOT" "$CONTAINERD_ROOT" "$DAGGER_CACHE"`},
		{"pnpm store directory", `install -d -m 0755 "$PNPM_STORE"`},
		{"pnpm store pin", `config set store-dir "$PNPM_STORE"`},
		{"module file", "/etc/modules-load.d/devbox.conf"},
		{"module load now", "modprobe br_netfilter"},
		{"module load overlay", "modprobe overlay"},
		{"docker keyring", "/etc/apt/keyrings/docker.asc"},
		{"docker repository", "/etc/apt/sources.list.d/docker.sources"},
		{"docker repository uri", "https://download.docker.com/linux/debian"},
		{"docker engine", "docker-ce"},
		{"containerd package", "containerd.io"},
		{"docker group", `usermod -aG docker "$LOGIN_USER"`},
		{"gcloud keyring", "/usr/share/keyrings/cloud.google.gpg"},
		{"gcloud repository", "/etc/apt/sources.list.d/google-cloud-sdk.sources"},
		{"gcloud package", "google-cloud-cli"},
		{"mise install", "MISE_INSTALL_PATH=/usr/local/bin/mise"},
		{"shim exposure", `ln -sfn "$shim" "/usr/local/bin/$(basename "$shim")"`},
		{"harness package", "HARNESS_PACKAGE='" + config.DefaultOmpPackage + "'"},
		{"harness version", "HARNESS_VERSION='" + config.DefaultOmpVersion + "'"},
		{"harness install", `install --global "$HARNESS_PACKAGE@$HARNESS_VERSION"`},
		{"harness link", `ln -sfn "$NPM_PREFIX/bin/$HARNESS_BINARY" "/usr/local/bin/$HARNESS_BINARY"`},
		{"stamp write", `date -u +%Y-%m-%dT%H:%M:%SZ > "$STAMP"`},
	}
	for _, item := range required {
		if !strings.Contains(script, item.want) {
			t.Errorf("startup script is missing %s: %q", item.what, item.want)
		}
	}
}

// chromiumPackages is written out here rather than read from the renderer: a
// library dropped from the list must fail this test, which it cannot do if the
// expectation and the implementation are the same value.
func TestRenderInstallsEveryChromiumLibrary(t *testing.T) {
	script, err := Render(config.Default())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	packages := []string{
		"fonts-liberation",
		"libasound2",
		"libatk-bridge2.0-0",
		"libatk1.0-0",
		"libcairo2",
		"libcups2",
		"libdbus-1-3",
		"libdrm2",
		"libgbm1",
		"libglib2.0-0",
		"libgtk-3-0",
		"libnspr4",
		"libnss3",
		"libpango-1.0-0",
		"libpangocairo-1.0-0",
		"libx11-6",
		"libx11-xcb1",
		"libxcb1",
		"libxcomposite1",
		"libxcursor1",
		"libxdamage1",
		"libxext6",
		"libxfixes3",
		"libxi6",
		"libxkbcommon0",
		"libxrandr2",
		"libxrender1",
		"libxshmfence1",
		"libxss1",
		"libxtst6",
		"xz-utils",
		"unzip",
	}
	for _, name := range packages {
		if !strings.Contains(script, "\t"+name+" \\\n") && !strings.Contains(script, "\t"+name+"\n") {
			t.Errorf("startup script does not install %s", name)
		}
	}
}

func TestRenderInstallsTheOperatorPackages(t *testing.T) {
	script, err := Render(config.Default())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, name := range []string{"build-essential", "gh", "git", "jq", "rsync", "tmux", "zellij"} {
		if !strings.Contains(script, "\t"+name+" \\\n") && !strings.Contains(script, "\t"+name+"\n") {
			t.Errorf("startup script does not install %s", name)
		}
	}
}

// The access layer checks a bootstrapped box with a non-interactive ssh command,
// which reads no shell profile. The shims are therefore reachable twice: through
// the links in /usr/local/bin, and through the PATH the environment file gives
// every session.
func TestRenderPutsTheShimsOnTheNonInteractivePath(t *testing.T) {
	script, err := Render(config.Default())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(script, "cat > /etc/environment <<ENVIRONMENT") {
		t.Error("the startup script does not write the session environment file")
	}
	want := `PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$LOGIN_HOME/.local/share/mise/shims"`
	if !strings.Contains(script, want) {
		t.Errorf("the session environment does not carry the shims directory:\n%s", want)
	}
}

// TestRenderStampGuard pins the shape of the guard: it tests the stamp file,
// takes the force from the environment, leaves before any phase runs, and is the
// only place the script writes the stamp.
func TestRenderStampGuard(t *testing.T) {
	script, err := Render(config.Default())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	guard := `if [ -f "$STAMP" ] && [ "${DEVBOX_BOOTSTRAP_FORCE:-0}" != 1 ]; then
	log "already bootstrapped, $STAMP exists; set DEVBOX_BOOTSTRAP_FORCE=1 to rebuild this box"
	exit 0
fi`
	guardIndex := strings.Index(script, guard)
	if guardIndex < 0 {
		t.Fatalf("startup script has no stamp guard:\n%s", script)
	}
	if first := strings.Index(script, "log 'phase base:"); first < 0 || first < guardIndex {
		t.Error("the stamp guard does not run before the first phase")
	}
	if strict := strings.Index(script, "set -euo pipefail"); strict < 0 || strict > guardIndex {
		t.Error("the stamp guard is not preceded by the strict mode setting")
	}

	// The stamp is written once, at the end: a machine that boots again after a
	// failure must run the whole script rather than trust a half-built box.
	write := `date -u +%Y-%m-%dT%H:%M:%SZ > "$STAMP"`
	if count := strings.Count(script, write); count != 1 {
		t.Fatalf("the stamp is written %d times, want exactly once", count)
	}
	if trimmed := strings.TrimSpace(script); !strings.HasSuffix(trimmed, write) {
		t.Errorf("the stamp is not the last statement:\n%s", script[len(script)-200:])
	}

	// Nothing removes a data path, and the one filesystem is created once, only
	// when the disk has none.
	if strings.Contains(script, "rm -rf") {
		t.Error("the startup script removes a path")
	}
	if count := strings.Count(script, "mkfs.ext4"); count != 1 {
		t.Errorf("the startup script formats a filesystem %d times, want once behind the blkid check", count)
	}
}

// TestRenderMatchesDeployArtifact keeps the published script and the generator
// from drifting: deploy/startup-script.sh is exactly what the renderer produces
// for the default configuration.
func TestRenderMatchesDeployArtifact(t *testing.T) {
	t.Setenv("USER", artifactUser)
	script, err := Render(config.Default())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	artifact, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read %s: %v", artifactPath, err)
	}
	if os.Getenv("DEVBOX_UPDATE_ARTIFACT") == "1" {
		// The artifact is generated, never hand-edited: change the template, then
		//   DEVBOX_UPDATE_ARTIFACT=1 go test -run TestRenderMatchesDeployArtifact ./internal/bootstrap/
		if err := os.WriteFile(artifactPath, []byte(script), 0o644); err != nil {
			t.Fatalf("update %s: %v", artifactPath, err)
		}
		return
	}
	if string(artifact) != script {
		t.Errorf("%s does not match Render(config.Default()): regenerate it with DEVBOX_UPDATE_ARTIFACT=1 go test -run TestRenderMatchesDeployArtifact ./internal/bootstrap/", artifactPath)
	}
	parseScript(t, string(artifact))
}

// TestRenderWithEmptyToolMap is the configuration a fresh operator has: no pins
// of their own. The script must still be a whole, valid script, and it installs
// the built-in pins because an empty table means the operator has not chosen.
func TestRenderWithEmptyToolMap(t *testing.T) {
	cfg := config.Default()
	cfg.Tools = map[string]string{}
	script, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(script, "mise use -g go@"+DefaultTools()["go"]) {
		t.Error("an empty tool map did not fall back to the built-in pins")
	}
	if !strings.HasSuffix(strings.TrimSpace(script), `date -u +%Y-%m-%dT%H:%M:%SZ > "$STAMP"`) {
		t.Error("an empty tool map produced a script that does not finish")
	}
	parseScript(t, script)
}

// TestRenderUsesConfiguredPins checks that the configuration wins: a pin the
// operator chose is rendered, and the built-in version for that tool is not.
func TestRenderUsesConfiguredPins(t *testing.T) {
	cfg := config.Default()
	cfg.Tools = map[string]string{"go": "1.25.0"}
	script, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(script, "mise use -g go@1.25.0") {
		t.Error("the configured pin for go is missing")
	}
	if strings.Contains(script, "mise use -g node@") {
		t.Error("a configured tool table rendered a tool it does not declare")
	}
}

func TestRenderRefusesAConfigurationItCannotScript(t *testing.T) {
	cases := []struct {
		what    string
		change  func(*config.Config)
		wantErr string
	}{
		{
			what:    "relative data mount",
			change:  func(c *config.Config) { c.DataMount = "mnt/data" },
			wantErr: "absolute path",
		},
		{
			what:    "missing data disk name",
			change:  func(c *config.Config) { c.DataDiskName = "" },
			wantErr: "data disk name",
		},
		{
			what:    "missing login user",
			change:  func(c *config.Config) { c.RemoteUser = "" },
			wantErr: "login user",
		},
		{
			what:    "missing harness",
			change:  func(c *config.Config) { c.OmpPackage = "" },
			wantErr: "harness",
		},
		{
			what:    "unpinned tool",
			change:  func(c *config.Config) { c.Tools = map[string]string{"go": ""} },
			wantErr: "pinned version",
		},
		{
			what:    "value the shell cannot hold",
			change:  func(c *config.Config) { c.DataMount = "/mnt/data;rm -rf /" },
			wantErr: "cannot quote",
		},
		{
			what:    "tool name the shell cannot hold",
			change:  func(c *config.Config) { c.Tools = map[string]string{"go; curl evil": "1.0.0"} },
			wantErr: "cannot quote",
		},
	}
	for _, item := range cases {
		t.Run(item.what, func(t *testing.T) {
			cfg := config.Default()
			item.change(&cfg)
			script, err := Render(cfg)
			if err == nil {
				t.Fatalf("Render accepted %s and produced %d bytes", item.what, len(script))
			}
			if !strings.Contains(err.Error(), item.wantErr) {
				t.Errorf("error %q does not mention %q", err, item.wantErr)
			}
			if script != "" {
				t.Errorf("Render returned a script along with the error: %q", script)
			}
		})
	}
}

func TestRenderCarriesNoSecrets(t *testing.T) {
	cfg := config.Default()
	cfg.SSHKey = "~/.ssh/id_ed25519_devbox"
	script, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(script, cfg.SSHKey) {
		t.Errorf("the startup script carries the operator's ssh key %q", cfg.SSHKey)
	}
	// The guard is for secret material, not for words: the script legitimately names
	// 1password-cli, which is a package rather than a value. What must never appear
	// is a key, or a value assigned to something secret-shaped.
	for _, pattern := range []struct {
		what string
		re   *regexp.Regexp
	}{
		{"a private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
		{"a value assigned to a secret-shaped name", regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|credential|auth)\s*=\s*\S`)},
		{"a long opaque token", regexp.MustCompile(`\b[A-Za-z0-9+/_-]{40,}={0,2}\b`)},
	} {
		if found := pattern.re.FindString(script); found != "" {
			t.Errorf("the startup script contains %s: %q", pattern.what, found)
		}
	}
}

// parseScript checks that a shell can read the whole script. Nothing runs: a
// parse catches a template that produced an unterminated quote or a broken
// heredoc, which is exactly the failure a byte comparison cannot see.
func parseScript(t *testing.T, script string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed, cannot parse the rendered script")
	}
	command := exec.Command(bash, "-n")
	command.Stdin = strings.NewReader(script)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("the rendered script does not parse: %v\n%s", err, out)
	}
}

// TestRenderIsDeterministic guards the artifact comparison: a map iterated in
// place of a sorted list would make two renderings of one configuration differ.
func TestRenderIsDeterministic(t *testing.T) {
	cfg := config.Default()
	cfg.Tools = map[string]string{"uv": "0.8.9", "go": "1.26.5", "node": "24.18.0", "jq": "1.8.2"}
	first, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for range 5 {
		again, err := Render(cfg)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if again != first {
			t.Fatal("two renderings of one configuration differ")
		}
	}
	goIndex := strings.Index(first, "mise use -g go@1.26.5")
	jqIndex := strings.Index(first, "mise use -g jq@1.8.2")
	if goIndex < 0 || jqIndex < 0 || goIndex > jqIndex {
		t.Error("the toolchain is not rendered in sorted order")
	}
}
