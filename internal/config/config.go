// Package config holds the operator's devbox settings and the paths devbox owns.
//
// The file is JSON so the standard library decoder can be strict about unknown
// fields: a typo in a setting fails loudly instead of silently provisioning a
// machine with the wrong shape.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Version is the configuration schema version.
const Version = 1

// Config is the operator's devbox configuration.
type Config struct {
	Version int    `json:"version"`
	Project string `json:"project"`
	Zone    string `json:"zone"`
	Region  string `json:"region"`

	MachineType  string `json:"machine_type"`
	ImageProject string `json:"image_project"`
	ImageFamily  string `json:"image_family"`

	BootDiskGB   int    `json:"boot_disk_gb"`
	BootDiskType string `json:"boot_disk_type"`
	DataDiskGB   int    `json:"data_disk_gb"`
	DataDiskType string `json:"data_disk_type"`
	DataDiskName string `json:"data_disk_name"`
	DataMount    string `json:"data_mount"`

	ServiceAccount string   `json:"service_account"`
	ExternalIP     bool     `json:"external_ip"`
	MaxRunDuration string   `json:"max_run_duration"`
	Tags           []string `json:"tags"`

	// BootstrapURL is the retained startup script the machine runs on every boot,
	// either a gs:// object or an https URL. A box created with an empty value
	// starts with no toolchain, which is only useful for a probe.
	BootstrapURL string `json:"bootstrap_url"`

	SSHPrefix   string `json:"ssh_prefix"`
	SSHKey      string `json:"ssh_key"`
	RemoteUser  string `json:"remote_user"`
	PortForward int    `json:"port_forward"`

	// Tools pins the mise-managed toolchain the bootstrap installs on the box.
	Tools map[string]string `json:"tools"`
	// OmpPackage and OmpVersion pin the agent harness the box runs.
	OmpPackage string `json:"omp_package"`
	OmpVersion string `json:"omp_version"`
}

// Default returns the shape this project recommends, matching the measured
// decisions: an Intel general-purpose machine, current Debian, a persistent SSD
// data disk (the fork primitive requires a disk a machine image can capture),
// and no external address.
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
	}
}

// Dir is the devbox state directory: configuration and mutation records live
// here so a reader can find everything devbox owns in one place.
func Dir() (string, error) {
	if base := os.Getenv("DEVBOX_HOME"); base != "" {
		return base, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "devbox"), nil
}

// DefaultPath is the configuration file inside Dir.
func DefaultPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// RecordDir is where durable mutation records are kept.
func RecordDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "records"), nil
}

// Load reads a configuration file and fills unset fields from Default.
func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration %s: %w", path, err)
	}
	defer file.Close()
	cfg := Default()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration %s: %w", path, err)
	}
	if cfg.Version == 0 {
		cfg.Version = Version
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("configuration %s: %w", path, err)
	}
	return cfg, nil
}

// LoadOrDefault reads the default configuration path, returning Default with a
// clear error when the file is absent so the operator knows what to create.
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

// Save writes the configuration with owner-only permissions.
func Save(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
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
		c.Region = RegionFromZone(c.Zone)
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
