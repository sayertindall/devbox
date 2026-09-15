// Package bootstrap renders and publishes the startup script that turns a stock
// Debian image into a working box, bakes a reusable image from a configured box,
// and reports the toolchain a box installs.
//
// The script is generated, never hand-edited: the renderer here is the single
// source of truth for what runs on first boot, and a test compares the checked-in
// artifact under deploy/ with the renderer so the two cannot drift.
package bootstrap

import (
	"fmt"
	"strings"
	"text/template"

	"devbox/internal/box"
	"devbox/internal/config"
)

// scriptValues is the substitution set for the startup script template. Blocks
// arrive pre-rendered and in sorted order, so one configuration always produces
// the same bytes.
type scriptValues struct {
	Log            string
	Stamp          string
	Mount          string
	Device         string
	DockerRoot     string
	ContainerdRoot string
	DaggerCache    string
	PNPMStore      string
	User           string
	HarnessPackage string
	HarnessVersion string
	HarnessBinary  string
	ToolBlock      string
	LibraryBlock   string
	PackageBlock   string
}

// Render returns the startup script for one configuration.
//
// The toolchain is the configuration's pins when it declares any, and the
// built-in pins otherwise, which is the same answer the tool commands give: a
// box built by this script and a box converged by `devbox tools apply` end up
// with the same versions.
func Render(cfg config.Config) (string, error) {
	values, err := scriptFor(cfg)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	if err := scriptTemplate.Execute(&out, values); err != nil {
		return "", fmt.Errorf("render startup script: %w", err)
	}
	return out.String(), nil
}

// scriptFor validates a configuration and turns it into template values. Every
// refusal happens here, before a byte of shell is produced: a script that
// half-works on a remote machine is worse than no script at all.
func scriptFor(cfg config.Config) (scriptValues, error) {
	if !strings.HasPrefix(cfg.DataMount, "/") {
		return scriptValues{}, fmt.Errorf("data mount %q must be an absolute path", cfg.DataMount)
	}
	if cfg.DataDiskName == "" {
		return scriptValues{}, fmt.Errorf("the data disk name is required to find the data disk")
	}
	if cfg.RemoteUser == "" {
		return scriptValues{}, fmt.Errorf("the login user is required to install the toolchain")
	}
	if cfg.OmpPackage == "" || cfg.OmpVersion == "" {
		return scriptValues{}, fmt.Errorf("the harness package and version are required to install the harness")
	}
	tools := cfg.EffectiveTools(DefaultTools())
	for _, name := range config.SortedTools(tools) {
		if tools[name] == "" {
			return scriptValues{}, fmt.Errorf("tool %s has no pinned version", name)
		}
		if err := checkSafe("tool name", name); err != nil {
			return scriptValues{}, err
		}
		if err := checkSafe("tool version", tools[name]); err != nil {
			return scriptValues{}, err
		}
	}
	values := scriptValues{
		Log:            box.BootstrapLog,
		Stamp:          box.BootstrapStamp,
		Mount:          cfg.DataMount,
		Device:         "/dev/disk/by-id/google-" + cfg.DataDiskName,
		DockerRoot:     box.DockerRoot,
		ContainerdRoot: box.ContainerdRoot,
		DaggerCache:    box.DaggerCache,
		PNPMStore:      box.PNPMStore,
		User:           cfg.RemoteUser,
		HarnessPackage: cfg.OmpPackage,
		HarnessVersion: cfg.OmpVersion,
		HarnessBinary:  DefaultHarnessBinary,
		ToolBlock:      toolBlock(MiseUseCommands(tools)),
		LibraryBlock:   aptBlock(chromiumLibraries()),
		PackageBlock:   aptBlock(systemPackages()),
	}
	for kind, value := range map[string]string{
		"data mount":    values.Mount,
		"data disk":     cfg.DataDiskName,
		"login user":    values.User,
		"harness":       values.HarnessPackage,
		"harness tag":   values.HarnessVersion,
		"harness entry": values.HarnessBinary,
	} {
		if err := checkSafe(kind, value); err != nil {
			return scriptValues{}, err
		}
	}
	return values, nil
}

// toolBlock renders the pinned toolchain as the commands the startup script
// runs, one per tool, as the login user so the shims and the mise data
// directory belong to the account the operator will use. The command itself is
// the canonical line `devbox tools apply` runs on a running box.
func toolBlock(commands []string) string {
	lines := make([]string, 0, len(commands))
	for _, command := range commands {
		lines = append(lines, "run_as_login "+command)
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// aptBlock renders packages as the argument list of one install, one per line,
// so a reader diffing two configurations sees which package moved.
func aptBlock(packages []string) string {
	return strings.Join(packages, " \\\n\t")
}

// chromiumLibraries are the shared libraries a headless Chromium needs. The
// harness drives a browser, and a missing library fails at launch with a message
// that names the library, not the command that needed it, so they are installed
// with the image rather than discovered on the box.
func chromiumLibraries() []string {
	return []string{
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
}

// systemPackages are the operator tools that come from Debian rather than from
// mise: they are either needed before mise exists, or they are what the box
// uses when nothing else is installed yet.
func systemPackages() []string {
	return []string{
		"build-essential",
		"gh",
		"git",
		"jq",
		"rsync",
		"tmux",
		"zellij",
	}
}

// shellSafe is the character set a rendered value may contain. The script is
// generated shell, so a value outside this set is refused rather than escaped: a
// tool name, a version, a mount, and a user name all fit, and a value that needs
// escaping means the configuration says something this renderer cannot express.
const shellSafe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._/@+:"

func checkSafe(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", kind)
	}
	for _, char := range value {
		if !strings.ContainsRune(shellSafe, char) {
			return fmt.Errorf("%s %q contains characters the startup script cannot quote", kind, value)
		}
	}
	return nil
}

// scriptTemplate is the startup script. Values are substituted inside single
// quotes, so the shell never expands them and a value cannot change the meaning
// of a line.
//
// Two invariants a reader cannot see from a single line:
//
//   - Every step is guarded by a file or command check, and every configuration
//     file is written whole rather than appended to, so a second run on a
//     half-built machine repairs what is missing and leaves the rest alone.
//   - Nothing removes a data path. The only destructive command is mkfs, and it
//     runs only when the data disk has no filesystem.
var scriptTemplate = template.Must(template.New("startup").Parse(`#!/bin/bash
# devbox bootstrap: turn a stock Debian image into a working box.
#
# Generated from the operator's configuration by devbox bootstrap show. Edit the
# renderer in internal/bootstrap and regenerate rather than editing this file.
set -euo pipefail

# The startup environment is not a login shell, so the PATH is set explicitly:
# the version of every tool on this box must come from the pins below and from
# nowhere else.
export PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
export DEBIAN_FRONTEND=noninteractive

LOG='{{.Log}}'
STAMP='{{.Stamp}}'
DATA_MOUNT='{{.Mount}}'
DATA_DEVICE='{{.Device}}'
DOCKER_ROOT='{{.DockerRoot}}'
CONTAINERD_ROOT='{{.ContainerdRoot}}'
DAGGER_CACHE='{{.DaggerCache}}'
PNPM_STORE='{{.PNPMStore}}'
LOGIN_USER='{{.User}}'
HARNESS_PACKAGE='{{.HarnessPackage}}'
HARNESS_VERSION='{{.HarnessVersion}}'
HARNESS_BINARY='{{.HarnessBinary}}'

log() {
	printf '%s devbox-bootstrap: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "$LOG"
}

# The guest agent runs a startup script as root, and everything below installs
# system packages or writes under /etc.
if [ "$(id -u)" -ne 0 ]; then
	echo 'devbox-bootstrap: must run as root' >&2
	exit 1
fi

# A machine that finished a bootstrap keeps its toolchain across a stop, a
# resume, and a reboot: the data disk holds the caches and the pins hold the
# versions. Re-running the long path would only recreate what is already there.
if [ -f "$STAMP" ] && [ "${DEVBOX_BOOTSTRAP_FORCE:-0}" != 1 ]; then
	log "already bootstrapped, $STAMP exists; set DEVBOX_BOOTSTRAP_FORCE=1 to rebuild this box"
	exit 0
fi

# Every user-scoped step runs as the login user, so the mise data directory, the
# shims, and the pnpm store belong to the account the operator will use.
run_as_login() {
	runuser -u "$LOGIN_USER" -- env HOME="$LOGIN_HOME" "$@"
}

# The mise shims are the entry points of the pinned toolchain. Linking them into
# /usr/local/bin puts node, pnpm, and every pinned tool on the PATH of a
# non-interactive ssh command, which never reads a shell profile.
expose_shims() {
	local shims="$LOGIN_HOME/.local/share/mise/shims"
	local shim
	if [ ! -d "$shims" ]; then
		return 0
	fi
	for shim in "$shims"/*; do
		if [ -e "$shim" ]; then
			ln -sfn "$shim" "/usr/local/bin/$(basename "$shim")"
		fi
	done
}

log 'phase base: installing the archive prerequisites'
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl e2fsprogs gnupg kmod

log "phase account: preparing the login user $LOGIN_USER"
if ! id -u "$LOGIN_USER" >/dev/null 2>&1; then
	# OS Login creates the operator's account on first login, which happens
	# after the first boot. Creating it here is what lets the harness land in a
	# home the operator will actually use.
	useradd --create-home --shell /bin/bash "$LOGIN_USER"
fi
LOGIN_HOME="$(getent passwd "$LOGIN_USER" | cut -d: -f6)"
if [ -z "$LOGIN_HOME" ] || [ ! -d "$LOGIN_HOME" ]; then
	log "cannot resolve the home directory of $LOGIN_USER"
	exit 1
fi

log 'phase modules: loading br_netfilter and overlay now and on every boot'
cat > /etc/modules-load.d/devbox.conf <<'MODULES'
br_netfilter
overlay
MODULES
modprobe br_netfilter
modprobe overlay

log "phase data disk: preparing $DATA_MOUNT on $DATA_DEVICE"
if [ ! -b "$DATA_DEVICE" ]; then
	log "the data disk $DATA_DEVICE is not attached"
	exit 1
fi
if ! blkid "$DATA_DEVICE" >/dev/null 2>&1; then
	log "no filesystem on $DATA_DEVICE, creating ext4"
	mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0 "$DATA_DEVICE"
fi
install -d -m 0755 "$DATA_MOUNT"
DATA_UUID="$(blkid -s UUID -o value "$DATA_DEVICE")"
if [ -z "$DATA_UUID" ]; then
	log "no filesystem UUID on $DATA_DEVICE, cannot persist the mount"
	exit 1
fi
if ! mountpoint -q "$DATA_MOUNT"; then
	mount "$DATA_DEVICE" "$DATA_MOUNT"
fi
# The mount is written by UUID so it survives a recreated device name, and nofail
# keeps a missing disk from blocking the boot. The line is added once: the check
# is what makes a second run harmless.
if ! grep -qs "UUID=$DATA_UUID" /etc/fstab; then
	printf 'UUID=%s %s ext4 defaults,nofail,discard 0 2\n' "$DATA_UUID" "$DATA_MOUNT" >> /etc/fstab
fi
install -d -m 0755 "$DOCKER_ROOT" "$CONTAINERD_ROOT" "$DAGGER_CACHE"
install -d -m 0755 "$PNPM_STORE"
chown "$LOGIN_USER" "$PNPM_STORE"

log 'phase docker: installing Docker CE from the Docker apt repository'
install -d -m 0755 /etc/apt/keyrings
if [ ! -f /etc/apt/keyrings/docker.asc ]; then
	curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
	chmod a+r /etc/apt/keyrings/docker.asc
fi
. /etc/os-release
cat > /etc/apt/sources.list.d/docker.sources <<DOCKER_SOURCES
Types: deb
URIs: https://download.docker.com/linux/debian
Suites: ${VERSION_CODENAME:-trixie}
Components: stable
Architectures: amd64
Signed-By: /etc/apt/keyrings/docker.asc
DOCKER_SOURCES
apt-get update
apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
# Images, containers, the containerd content store, and the Dagger cache live on
# the data disk: the boot disk and any local SSD are gone after a stop or a
# resume, and rebuilding them is the slowest part of a box.
install -d -m 0755 /etc/docker /etc/containerd
cat > /etc/docker/daemon.json <<DOCKER_DAEMON
{
  "data-root": "${DOCKER_ROOT}"
}
DOCKER_DAEMON
cat > /etc/containerd/config.toml <<CONTAINERD_CONFIG
version = 2
root = "${CONTAINERD_ROOT}"
CONTAINERD_CONFIG
systemctl enable containerd docker
systemctl restart containerd
systemctl restart docker
usermod -aG docker "$LOGIN_USER"

log 'phase libraries: installing the shared libraries a headless Chromium needs'
apt-get install -y --no-install-recommends \
	{{.LibraryBlock}}

log 'phase packages: installing the operator tools'
apt-get install -y --no-install-recommends \
	{{.PackageBlock}}

log 'phase cloud cli: installing the Google Cloud CLI from its apt repository'
if [ ! -f /usr/share/keyrings/cloud.google.gpg ]; then
	curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg | gpg --dearmor --yes -o /usr/share/keyrings/cloud.google.gpg
fi
cat > /etc/apt/sources.list.d/google-cloud-sdk.sources <<'GCLOUD_SOURCES'
Types: deb
URIs: https://packages.cloud.google.com/apt
Suites: cloud-sdk
Components: main
Architectures: amd64
Signed-By: /usr/share/keyrings/cloud.google.gpg
GCLOUD_SOURCES
apt-get update
apt-get install -y --no-install-recommends google-cloud-cli

log 'phase toolchain: installing mise'
if [ ! -x /usr/local/bin/mise ]; then
	curl -fsSL https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh
fi
log 'phase tools: installing the pinned toolchain'
{{.ToolBlock}}expose_shims
if [ -x "$LOGIN_HOME/.local/share/mise/shims/pnpm" ]; then
	run_as_login "$LOGIN_HOME/.local/share/mise/shims/pnpm" config set store-dir "$PNPM_STORE"
fi

log "phase harness: installing $HARNESS_PACKAGE at $HARNESS_VERSION with npm"
NPM_SHIM="$LOGIN_HOME/.local/share/mise/shims/npm"
if [ ! -x "$NPM_SHIM" ]; then
	log 'npm is not available, cannot install the harness'
	exit 1
fi
run_as_login "$NPM_SHIM" install --global "$HARNESS_PACKAGE@$HARNESS_VERSION"
NPM_PREFIX="$(run_as_login "$NPM_SHIM" prefix --global)"
if [ ! -x "$NPM_PREFIX/bin/$HARNESS_BINARY" ]; then
	log "the harness executable $HARNESS_BINARY is missing from $NPM_PREFIX/bin"
	exit 1
fi
# npm installs into the node prefix, which is not on the PATH of a
# non-interactive ssh command, so the harness is linked where the shims are.
ln -sfn "$NPM_PREFIX/bin/$HARNESS_BINARY" "/usr/local/bin/$HARNESS_BINARY"
expose_shims

log 'phase complete: this box is ready'
install -d -m 0755 "$(dirname "$STAMP")"
# The stamp is the last thing written, so a machine that boots again after any
# failure above runs the whole script instead of trusting a half-built box.
date -u +%Y-%m-%dT%H:%M:%SZ > "$STAMP"
`))
