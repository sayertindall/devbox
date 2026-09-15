package box

// Remote paths are fixed by contract so the bootstrap slice installs what the
// tree and agent slices expect. A box that moves one of these without the others
// breaks silently, so they live in one place and the bootstrap script is checked
// against them by test.
const (
	// StateRoot is where devbox keeps trees and agent sessions on the box.
	StateRoot = "devbox"
	// TreeRoot holds every pushed tree, one directory per tree name.
	TreeRoot = StateRoot + "/trees"
	// AgentRoot holds one directory per agent session.
	AgentRoot = StateRoot + "/agents"
	// DockerRoot is the Docker data root, on the data disk so images and the
	// build cache survive a stop and a resume.
	DockerRoot = "/mnt/data/docker"
	// ContainerdRoot is the containerd content store. It lives outside the Docker
	// data root on current engines, and forgetting it fills the boot disk.
	ContainerdRoot = "/mnt/data/containerd"
	// DaggerCache is the engine cache the Dagger CLI mounts.
	DaggerCache = "/mnt/data/dagger"
	// PNPMStore is the pnpm content-addressable store.
	PNPMStore = "/mnt/data/pnpm-store"
	// BootstrapLog is where the startup script records what it did on this boot.
	BootstrapLog = "/var/log/devbox-bootstrap.log"
	// BootstrapStamp is written after a successful first boot, so a later boot
	// can skip the long path.
	BootstrapStamp = "/var/lib/devbox/bootstrap.ok"
)

// SSHConfigMarkerStart and SSHConfigMarkerEnd delimit the devbox-managed block in
// the operator's SSH configuration. Everything outside the block is the
// operator's own and is never rewritten.
const (
	SSHConfigMarkerStart = "# >>> devbox managed block >>>"
	SSHConfigMarkerEnd   = "# <<< devbox managed block <<<"
)
