// Package config holds the operator's devbox settings and the paths devbox owns.
//
// The file is TOML at ~/.devbox/config.toml, and it is meant to be edited by
// hand: Save writes a commented canonical version, while the tool commands edit
// the tool table in place so a hand-written comment survives an update.
//
// Decoding is strict. An unknown key is an error, because a typo in a setting
// must fail loudly instead of silently provisioning a machine with the wrong
// shape.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Version is the configuration schema version.
const Version = 1

// Config is the operator's devbox configuration. Field names are the domain
// words; the TOML keys are their snake_case spelling.
type Config struct {
	Version int    `toml:"version"`
	Project string `toml:"project"`
	Zone    string `toml:"zone"`
	Region  string `toml:"region"`

	MachineType  string `toml:"machine_type"`
	ImageProject string `toml:"image_project"`
	ImageFamily  string `toml:"image_family"`

	BootDiskGB   int    `toml:"boot_disk_gb"`
	BootDiskType string `toml:"boot_disk_type"`
	DataDiskGB   int    `toml:"data_disk_gb"`
	DataDiskType string `toml:"data_disk_type"`
	DataDiskName string `toml:"data_disk_name"`
	DataMount    string `toml:"data_mount"`

	ServiceAccount string   `toml:"service_account"`
	ExternalIP     bool     `toml:"external_ip"`
	MaxRunDuration string   `toml:"max_run_duration"`
	Tags           []string `toml:"tags"`

	// BootstrapURL is the retained startup script the machine runs on every boot,
	// either a gs:// object or an https URL. A box created with an empty value
	// starts with no toolchain, which is only useful for a probe.
	BootstrapURL string `toml:"bootstrap_url"`

	SSHPrefix   string `toml:"ssh_prefix"`
	SSHKey      string `toml:"ssh_key"`
	RemoteUser  string `toml:"remote_user"`
	PortForward int    `toml:"port_forward"`

	// Tools pins the mise-managed toolchain every box installs. This table is
	// edited by `devbox tools`, which also applies it to a running box.
	Tools map[string]string `toml:"tools"`

	// OmpPackage and OmpVersion pin the agent harness the box runs.
	OmpPackage string `toml:"omp_package"`
	OmpVersion string `toml:"omp_version"`
}

// Built-in defaults for the agent harness. They are used when the configuration
// leaves the harness keys empty, so a fresh configuration produces a working box
// without the operator having to look up a version first.
const (
	DefaultOmpPackage = "@oh-my-pi/pi-coding-agent"
	DefaultOmpVersion = "18.1.22"
)

// Default returns the shape this project recommends, matching the measured
// decisions: an Intel general-purpose machine, current Debian, a persistent SSD
// data disk (machine images cannot capture an instance with Hyperdisk attached,
// and forkability is the point), and no external address.
func Default() Config {
	return Config{
		Version:        Version,
		Zone:           "us-central1-a",
		Region:         "us-central1",
		MachineType:    "n2-standard-16",
		ImageProject:   "debian-cloud",
		ImageFamily:    "debian-13",
		BootDiskGB:     100,
		BootDiskType:   "pd-balanced",
		DataDiskGB:     1024,
		DataDiskType:   "pd-ssd",
		DataDiskName:   "devbox-data",
		DataMount:      "/mnt/data",
		ExternalIP:     false,
		MaxRunDuration: "12h",
		Tags:           []string{"devbox"},
		SSHPrefix:      "devbox-",
		RemoteUser:     os.Getenv("USER"),
		PortForward:    9224,
		Tools:          map[string]string{},
		OmpPackage:     DefaultOmpPackage,
		OmpVersion:     DefaultOmpVersion,
	}
}

// EffectiveTools is the toolchain a box installs: the configuration's pins when
// it declares any, and the built-in pins otherwise. There is exactly one rule,
// so an empty table cannot mean two different things to two commands.
func (c Config) EffectiveTools(builtins map[string]string) map[string]string {
	if len(c.Tools) > 0 {
		return c.Tools
	}
	return builtins
}

// EffectiveOmpPackage returns the harness package, falling back to the built-in.
func (c Config) EffectiveOmpPackage() string {
	if c.OmpPackage != "" {
		return c.OmpPackage
	}
	return DefaultOmpPackage
}

// EffectiveOmpVersion returns the harness version, falling back to the built-in.
func (c Config) EffectiveOmpVersion() string {
	if c.OmpVersion != "" {
		return c.OmpVersion
	}
	return DefaultOmpVersion
}

// Dir is the devbox state directory. Configuration, records, trees state, and
// agent state live under it so a reader finds everything devbox owns in one
// place. DEVBOX_HOME overrides it for tests and for a second checkout.
func Dir() (string, error) {
	if base := os.Getenv("DEVBOX_HOME"); base != "" {
		return base, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".devbox"), nil
}

// StatePath joins parts under the state directory. It creates nothing: the
// caller owns directory creation for what it stores.
func StatePath(parts ...string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	for _, part := range parts {
		if part == "" || strings.ContainsAny(part, `/\`) || part == "." || part == ".." {
			return "", fmt.Errorf("invalid state path component %q", part)
		}
	}
	return filepath.Join(append([]string{dir}, parts...)...), nil
}

// DefaultPath is the configuration file inside Dir.
func DefaultPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// RecordDir is where durable mutation records are kept.
func RecordDir() (string, error) {
	return StatePath("records")
}

// Load reads a configuration file and fills unset fields from Default.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read configuration %s: %w", path, err)
	}
	cfg, err := Decode(data)
	if err != nil {
		return Config{}, fmt.Errorf("configuration %s: %w", path, err)
	}
	return cfg, nil
}

// Decode parses configuration bytes with unknown keys rejected.
func Decode(data []byte) (Config, error) {
	cfg := Default()
	decoder := toml.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	if cfg.Version == 0 {
		cfg.Version = Version
	}
	if cfg.Tools == nil {
		cfg.Tools = map[string]string{}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadOrDefault reads the given path, or the default path when empty.
func LoadOrDefault(explicit string) (Config, string, error) {
	path := explicit
	if path == "" {
		resolved, err := DefaultPath()
		if err != nil {
			return Config{}, "", err
		}
		path = resolved
	}
	cfg, err := Load(path)
	if err != nil {
		return Config{}, path, err
	}
	return cfg, path, nil
}

// Save writes the canonical commented configuration. Nothing else about the
// operator's file is preserved, so Save is only used to create or rewrite it;
// targeted edits go through the tool functions in tools.go.
func Save(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	return writeFile(path, []byte(Render(cfg)))
}

// Render produces the commented configuration file for the settings.
func Render(cfg Config) string {
	var out strings.Builder
	out.WriteString("# devbox configuration.\n")
	out.WriteString("#\n")
	out.WriteString("# Edit this file by hand, or use `devbox tools` to manage the toolchain:\n")
	out.WriteString("#   devbox tools add ripgrep          resolve the newest version and pin it\n")
	out.WriteString("#   devbox tools update               refresh every pin in [tools]\n")
	out.WriteString("#   devbox tools apply <box>          converge a running box without recreating it\n")
	out.WriteString("#\n")
	out.WriteString("# `devbox config show` prints the effective settings and this path.\n\n")

	fmt.Fprintf(&out, "version = %d\n", cfg.Version)
	fmt.Fprintf(&out, "project = %s\n", quote(cfg.Project))
	out.WriteString("# zone decides the region below it; a box and its data disk live in one zone\n")
	fmt.Fprintf(&out, "zone = %s\n", quote(cfg.Zone))
	fmt.Fprintf(&out, "region = %s\n\n", quote(cfg.Region))

	fmt.Fprintf(&out, "machine_type = %s\n", quote(cfg.MachineType))
	fmt.Fprintf(&out, "image_project = %s\n", quote(cfg.ImageProject))
	fmt.Fprintf(&out, "image_family = %s\n", quote(cfg.ImageFamily))

	fmt.Fprintf(&out, "boot_disk_gb = %d\n", cfg.BootDiskGB)
	fmt.Fprintf(&out, "boot_disk_type = %s\n", quote(cfg.BootDiskType))
	out.WriteString("# the data disk carries the docker root, the containerd root, the dagger\n")
	out.WriteString("# cache, and the pnpm store, because local SSD is discarded on stop\n")
	fmt.Fprintf(&out, "data_disk_gb = %d\n", cfg.DataDiskGB)
	out.WriteString("# pd-ssd, not hyperdisk: a machine image cannot be captured from an instance\n")
	out.WriteString("# with a hyperdisk attached, and fork needs a machine image\n")
	fmt.Fprintf(&out, "data_disk_type = %s\n", quote(cfg.DataDiskType))
	fmt.Fprintf(&out, "data_disk_name = %s\n", quote(cfg.DataDiskName))
	fmt.Fprintf(&out, "data_mount = %s\n\n", quote(cfg.DataMount))

	fmt.Fprintf(&out, "service_account = %s\n", quote(cfg.ServiceAccount))
	out.WriteString("# false means no external address: SSH goes through an IAP tunnel and egress\n")
	out.WriteString("# needs the cloud router and NAT that `devbox network ensure` creates\n")
	fmt.Fprintf(&out, "external_ip = %t\n", cfg.ExternalIP)
	out.WriteString("# a cap with a stop action, so a forgotten box pauses instead of running all night\n")
	fmt.Fprintf(&out, "max_run_duration = %s\n", quote(cfg.MaxRunDuration))
	fmt.Fprintf(&out, "tags = %s\n\n", array(cfg.Tags))

	fmt.Fprintf(&out, "bootstrap_url = %s\n\n", quote(cfg.BootstrapURL))

	fmt.Fprintf(&out, "ssh_prefix = %s\n", quote(cfg.SSHPrefix))
	fmt.Fprintf(&out, "ssh_key = %s\n", quote(cfg.SSHKey))
	fmt.Fprintf(&out, "remote_user = %s\n", quote(cfg.RemoteUser))
	fmt.Fprintf(&out, "port_forward = %d\n\n", cfg.PortForward)

	fmt.Fprintf(&out, "omp_package = %s\n", quote(cfg.OmpPackage))
	fmt.Fprintf(&out, "omp_version = %s\n\n", quote(cfg.OmpVersion))

	out.WriteString("# The toolchain installed on every box, resolved by mise. Versions are exact:\n")
	out.WriteString("# a floating version would make two boxes built a week apart disagree.\n")
	out.WriteString("[tools]\n")
	for _, name := range SortedTools(cfg.Tools) {
		fmt.Fprintf(&out, "%s = %s\n", toolKey(name), quote(cfg.Tools[name]))
	}
	return out.String()
}

// SortedTools returns the tool names in a stable order.
func SortedTools(tools map[string]string) []string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// toolKey renders a TOML key, quoting a name that is not a bare key.
func toolKey(name string) string {
	bare := name != ""
	for index := 0; index < len(name); index++ {
		char := name[index]
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
			continue
		}
		bare = false
		break
	}
	if bare {
		return name
	}
	return quote(name)
}

func quote(value string) string { return strconv.Quote(value) }

func array(values []string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, quote(value))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Validate rejects a configuration that cannot provision a correct machine.
func (c Config) Validate() error {
	if c.Project == "" {
		return fmt.Errorf("project is required")
	}
	if c.Zone == "" {
		return fmt.Errorf("zone is required")
	}
	if c.Region == "" {
		return fmt.Errorf("region is required")
	}
	if c.MachineType == "" {
		return fmt.Errorf("machine_type is required")
	}
	if c.ImageProject == "" || c.ImageFamily == "" {
		return fmt.Errorf("image_project and image_family are required")
	}
	if c.BootDiskGB <= 0 || c.DataDiskGB <= 0 {
		return fmt.Errorf("disk sizes must be positive")
	}
	if c.DataDiskType != "pd-ssd" && c.DataDiskType != "pd-balanced" && c.DataDiskType != "hyperdisk-balanced" {
		return fmt.Errorf("unsupported data_disk_type %q", c.DataDiskType)
	}
	if !strings.HasPrefix(c.DataMount, "/") {
		return fmt.Errorf("data_mount must be an absolute path")
	}
	if c.ServiceAccount == "" {
		return fmt.Errorf("service_account is required")
	}
	if c.DataDiskName == "" {
		return fmt.Errorf("data_disk_name is required")
	}
	if c.SSHPrefix == "" {
		return fmt.Errorf("ssh_prefix is required")
	}
	if c.RemoteUser == "" {
		return fmt.Errorf("remote_user is required")
	}
	if c.PortForward <= 0 || c.PortForward > 65535 {
		return fmt.Errorf("port_forward must be a valid port")
	}
	if c.OmpPackage == "" || c.OmpVersion == "" {
		return fmt.Errorf("omp_package and omp_version are required")
	}
	for name, version := range c.Tools {
		if err := validateToolName(name); err != nil {
			return err
		}
		if err := validateVersion(version); err != nil {
			return fmt.Errorf("tool %s: %w", name, err)
		}
	}
	return nil
}

// RegionFromZone turns us-central1-a into us-central1.
func RegionFromZone(zone string) string {
	index := strings.LastIndex(zone, "-")
	if index <= 0 {
		return zone
	}
	return zone[:index]
}

// SSHHost is the SSH alias for a box, so every tool on the operator's machine
// (ssh, scp, rsync, Zed, VS Code) reaches it through one definition.
func (c Config) SSHHost(name string) string {
	return c.SSHPrefix + name
}

// ProjectFlag renders the project argument used by every gcloud invocation.
func (c Config) ProjectFlag() string { return "--project=" + c.Project }

// ZoneFlag renders the zone argument used by every zonal gcloud invocation.
func (c Config) ZoneFlag() string { return "--zone=" + c.Zone }

func writeFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create configuration temporary file: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("set configuration permissions: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write configuration: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync configuration: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close configuration: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("install configuration: %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open configuration directory: %w", err)
	}
	defer directory.Close()
	return directory.Sync()
}
