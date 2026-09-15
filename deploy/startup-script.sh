#!/bin/bash
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

LOG='/var/log/devbox-bootstrap.log'
STAMP='/var/lib/devbox/bootstrap.ok'
DATA_MOUNT='/mnt/data'
DATA_DEVICE='/dev/disk/by-id/google-devbox-data'
DOCKER_ROOT='/mnt/data/docker'
CONTAINERD_ROOT='/mnt/data/containerd'
DAGGER_CACHE='/mnt/data/dagger'
PNPM_STORE='/mnt/data/pnpm-store'
LOGIN_USER='sayertindall'
HARNESS_PACKAGE='@oh-my-pi/pi-coding-agent'
HARNESS_VERSION='18.1.22'
HARNESS_BINARY='omp'

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
  "data-root": "/mnt/data/docker"
}
DOCKER_DAEMON
cat > /etc/containerd/config.toml <<CONTAINERD_CONFIG
version = 2
root = "/mnt/data/containerd"
CONTAINERD_CONFIG
systemctl enable containerd docker
systemctl restart containerd
systemctl restart docker
usermod -aG docker "$LOGIN_USER"

log 'phase libraries: installing the shared libraries a headless Chromium needs'
apt-get install -y --no-install-recommends \
	fonts-liberation \
	libasound2 \
	libatk-bridge2.0-0 \
	libatk1.0-0 \
	libcairo2 \
	libcups2 \
	libdbus-1-3 \
	libdrm2 \
	libgbm1 \
	libglib2.0-0 \
	libgtk-3-0 \
	libnspr4 \
	libnss3 \
	libpango-1.0-0 \
	libpangocairo-1.0-0 \
	libx11-6 \
	libx11-xcb1 \
	libxcb1 \
	libxcomposite1 \
	libxcursor1 \
	libxdamage1 \
	libxext6 \
	libxfixes3 \
	libxi6 \
	libxkbcommon0 \
	libxrandr2 \
	libxrender1 \
	libxshmfence1 \
	libxss1 \
	libxtst6 \
	xz-utils \
	unzip

log 'phase packages: installing the operator tools'
apt-get install -y --no-install-recommends \
	build-essential \
	gh \
	git \
	jq \
	rsync \
	tmux \
	zellij

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
run_as_login mise use -g conftest@0.70.0
run_as_login mise use -g cosign@3.1.3
run_as_login mise use -g cue@0.17.1
run_as_login mise use -g dagger@0.21.9
run_as_login mise use -g flux2@2.9.5
run_as_login mise use -g gh@2.100.0
run_as_login mise use -g go@1.26.5
run_as_login mise use -g helm@3.16.3
run_as_login mise use -g hunk@0.19.0
run_as_login mise use -g jq@1.8.2
run_as_login mise use -g just@1.42.4
run_as_login mise use -g kubeconform@0.8.0
run_as_login mise use -g kubectl@1.34.11
run_as_login mise use -g node@24.18.0
run_as_login mise use -g opentofu@1.12.6
run_as_login mise use -g oras@1.3.4
run_as_login mise use -g pnpm@12.4.1
run_as_login mise use -g python@3.12.14
run_as_login mise use -g rust@1.98.0
run_as_login mise use -g talosctl@1.14.0
run_as_login mise use -g uv@0.8.9
expose_shims
# A non-interactive ssh command runs bash -c, which reads no profile, so the
# shims directory is also put on the PATH of every session through
# /etc/environment. The links in /usr/local/bin stay as the guarantee: they
# resolve a tool even where an environment file is not applied. devbox owns this
# file on a box it built, so it is written whole.
cat > /etc/environment <<ENVIRONMENT
PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$LOGIN_HOME/.local/share/mise/shims"
ENVIRONMENT
if [ -x "$LOGIN_HOME/.local/share/mise/shims/pnpm" ]; then
	run_as_login "$LOGIN_HOME/.local/share/mise/shims/pnpm" config set store-dir "$PNPM_STORE"
fi

log "phase harness: installing $HARNESS_PACKAGE at $HARNESS_VERSION with npm"
NPM_SHIM="$LOGIN_HOME/.local/share/mise/shims/npm"
if [ ! -x "$NPM_SHIM" ]; then
	log 'npm is not available: pin node in [tools] so the harness can be installed'
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

log 'phase complete: this box is ready'
install -d -m 0755 "$(dirname "$STAMP")"
# The stamp is the last thing written, so a machine that boots again after any
# failure above runs the whole script instead of trusting a half-built box.
date -u +%Y-%m-%dT%H:%M:%SZ > "$STAMP"
