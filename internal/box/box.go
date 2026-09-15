// Package box names and describes one devbox machine.
//
// A box is a Compute Engine instance that devbox created, labeled, and can fork.
// The name grammar is strict because the name becomes an instance name, a disk
// name, an image name, an SSH alias, and part of a Cloud Storage path.
package box

import (
	"devbox/internal/gcloud"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Name is a validated box name.
type Name string

// labelName and labelManaged mark instances devbox owns, so list and destroy
// never touch a machine this tool did not create.
const (
	labelName    = "devbox-name"
	labelManaged = "devbox-managed"
)

// ParseName enforces the grammar every derived resource name depends on.
func ParseName(value string) (Name, error) {
	if len(value) == 0 || len(value) > 30 {
		return "", fmt.Errorf("box name must be 1 to 30 characters")
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		lower := char >= 'a' && char <= 'z'
		digit := char >= '0' && char <= '9'
		if index == 0 && !lower {
			return "", fmt.Errorf("box name must start with a lowercase letter")
		}
		if !lower && !digit && char != '-' {
			return "", fmt.Errorf("box name may contain only lowercase letters, digits, and hyphens")
		}
	}
	if strings.HasSuffix(value, "-") {
		return "", fmt.Errorf("box name may not end with a hyphen")
	}
	return Name(value), nil
}

func (n Name) String() string { return string(n) }

// Labels returns the labels every devbox instance carries.
func Labels(name Name) map[string]string {
	return map[string]string{labelName: name.String(), labelManaged: "true"}
}

// LabelFlag renders labels as the key=value list gcloud expects, ordered so the
// argument vector is stable across runs and therefore readable in a record.
func LabelFlag(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}

// NameFromLabels recovers the box name from an instance's labels.
func NameFromLabels(labels map[string]string) (Name, bool) {
	if labels == nil {
		return "", false
	}
	if labels[labelManaged] != "true" {
		return "", false
	}
	value, ok := labels[labelName]
	if !ok {
		return "", false
	}
	name, err := ParseName(value)
	if err != nil {
		return "", false
	}
	return name, true
}

// Facts is what devbox knows about a box after reading it back from the cloud.
type Facts struct {
	Name        string            `json:"name"`
	Zone        string            `json:"zone"`
	MachineType string            `json:"machineType"`
	Status      string            `json:"status"`
	CreatedAt   time.Time         `json:"creationTimestamp"`
	Labels      map[string]string `json:"labels"`
	Interfaces  []interfaceRef    `json:"networkInterfaces"`
	Disks       []attachedDisk    `json:"disks"`
}

type interfaceRef struct {
	NetworkIP     string `json:"networkIP"`
	AccessConfigs []struct {
		NatIP string `json:"natIP"`
	} `json:"accessConfigs"`
}

type attachedDisk struct {
	DeviceName string `json:"deviceName"`
	Source     string `json:"source"`
	Boot       bool   `json:"boot"`
}

// Decode parses the JSON gcloud returns for a describe or a list call. A
// describe is one object and a list is an array of them, so both shapes arrive
// here and leave as a slice.
func Decode(data string) ([]Facts, error) {
	normalized, err := gcloud.Resources(data)
	if err != nil {
		return nil, fmt.Errorf("decode instance description: %w", err)
	}
	var facts []Facts
	if err := json.Unmarshal(normalized, &facts); err != nil {
		return nil, fmt.Errorf("decode instance description: %w", err)
	}
	return facts, nil
}

// Fact is the instance from a describe, which gcloud reports as one object.
func Fact(data string) (Facts, error) {
	all, err := Decode(data)
	if err != nil {
		return Facts{}, err
	}
	if len(all) == 0 {
		return Facts{}, fmt.Errorf("instance was not found")
	}
	return all[0], nil
}

// ZoneName reduces a zone URL to its short name.
func ZoneName(zone string) string {
	if zone == "" {
		return ""
	}
	parts := strings.Split(zone, "/")
	return parts[len(parts)-1]
}

// MachineTypeName reduces a machine type URL to its short name.
func MachineTypeName(machineType string) string {
	if machineType == "" {
		return ""
	}
	parts := strings.Split(machineType, "/")
	return parts[len(parts)-1]
}

// InternalIP is the primary internal address, or empty when absent.
func (f Facts) InternalIP() string {
	if len(f.Interfaces) == 0 {
		return ""
	}
	return f.Interfaces[0].NetworkIP
}

// ExternalIP is the primary external address, or empty when the box has none.
func (f Facts) ExternalIP() string {
	if len(f.Interfaces) == 0 {
		return ""
	}
	if len(f.Interfaces[0].AccessConfigs) == 0 {
		return ""
	}
	return f.Interfaces[0].AccessConfigs[0].NatIP
}

// DataDisk reports the device name of the attached data disk, or empty when the
// box has none.
func (f Facts) DataDisk() string {
	for _, disk := range f.Disks {
		if disk.Boot {
			continue
		}
		return disk.DeviceName
	}
	return ""
}

// Running reports whether the instance is in a state that accepts work.
func (f Facts) Running() bool { return f.Status == "RUNNING" }
