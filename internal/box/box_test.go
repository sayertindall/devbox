package box

import (
	"strings"
	"testing"
)

func TestParseNameAcceptsTheGrammar(t *testing.T) {
	for _, valid := range []string{"dev", "dev-2", "a1", strings.Repeat("a", 30)} {
		if _, err := ParseName(valid); err != nil {
			t.Fatalf("%q should be a valid box name: %v", valid, err)
		}
	}
}

func TestParseNameRejectsWhatWouldBreakADerivedResource(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"uppercase":      "Dev",
		"leading digit":  "1dev",
		"leading hyphen": "-dev",
		"trailing -":     "dev-",
		"underscore":     "my_box",
		"dot":            "my.box",
		"slash":          "my/box",
		"space":          "my box",
		"too long":       strings.Repeat("a", 31),
	}
	for label, value := range cases {
		if _, err := ParseName(value); err == nil {
			t.Fatalf("%s: %q must be rejected", label, value)
		}
	}
}

func TestLabelsRoundTripThroughNameFromLabels(t *testing.T) {
	name, err := ParseName("dev")
	if err != nil {
		t.Fatal(err)
	}
	labels := Labels(name)
	recovered, ok := NameFromLabels(labels)
	if !ok || recovered != name {
		t.Fatalf("labels did not identify the box: %+v", labels)
	}
}

func TestNameFromLabelsRefusesAMachineDevboxDidNotCreate(t *testing.T) {
	if _, ok := NameFromLabels(map[string]string{"devbox-name": "dev"}); ok {
		t.Fatal("a machine without the managed label must not be treated as a box")
	}
	if _, ok := NameFromLabels(map[string]string{"devbox-managed": "true"}); ok {
		t.Fatal("a machine without a box name must not be treated as a box")
	}
	if _, ok := NameFromLabels(nil); ok {
		t.Fatal("an unlabeled machine must not be treated as a box")
	}
}

func TestLabelFlagIsStable(t *testing.T) {
	name, err := ParseName("dev")
	if err != nil {
		t.Fatal(err)
	}
	first := LabelFlag(Labels(name))
	second := LabelFlag(Labels(name))
	if first != second {
		t.Fatalf("labels must render in a stable order so a recorded vector is readable: %q vs %q", first, second)
	}
	if first != "devbox-managed=true,devbox-name=dev" {
		t.Fatalf("unexpected label rendering: %q", first)
	}
}

func TestFactsReadTheInstanceDescription(t *testing.T) {
	raw := `{
		"name": "dev",
		"zone": "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a",
		"machineType": "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/machineTypes/n2-standard-16",
		"status": "RUNNING",
		"labels": {"devbox-managed": "true", "devbox-name": "dev"},
		"networkInterfaces": [{"networkIP": "10.0.0.2", "accessConfigs": []}],
		"disks": [
			{"deviceName": "persistent-disk-0", "boot": true},
			{"deviceName": "devbox-data", "boot": false}
		]
	}`
	facts, err := Fact(raw)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Name != "dev" || !facts.Running() {
		t.Fatalf("facts did not decode: %+v", facts)
	}
	if ZoneName(facts.Zone) != "us-central1-a" {
		t.Fatalf("zone url did not reduce: %q", facts.Zone)
	}
	if MachineTypeName(facts.MachineType) != "n2-standard-16" {
		t.Fatalf("machine type url did not reduce: %q", facts.MachineType)
	}
	if facts.InternalIP() != "10.0.0.2" {
		t.Fatalf("internal address missing: %q", facts.InternalIP())
	}
	if facts.ExternalIP() != "" {
		t.Fatalf("a box with no access config must report no external address: %q", facts.ExternalIP())
	}
	if facts.DataDisk() != "devbox-data" {
		t.Fatalf("data disk missing: %q", facts.DataDisk())
	}
}

// TestDecodeAcceptsBothShapes covers the two answers gcloud gives: a describe
// returns one object and a list returns an array of them, and a caller that
// decodes a slice must handle either without knowing which verb ran.
func TestDecodeAcceptsBothShapes(t *testing.T) {
	described := `{"name":"dev","status":"RUNNING","labels":{"devbox-managed":"true","devbox-name":"dev"}}`
	listed := `[` + described + `,{"name":"other","status":"STOPPED"}]`

	one, err := Fact(described)
	if err != nil {
		t.Fatalf("a described instance must decode: %v", err)
	}
	if one.Name != "dev" || !one.Running() {
		t.Fatalf("facts did not decode: %+v", one)
	}
	all, err := Decode(listed)
	if err != nil {
		t.Fatalf("a listed project must decode: %v", err)
	}
	if len(all) != 2 || all[1].Name != "other" {
		t.Fatalf("list did not decode: %+v", all)
	}
}

func TestFactRejectsAnEmptyDescription(t *testing.T) {
	if _, err := Fact("[]"); err == nil {
		t.Fatal("an empty describe result means the instance is gone and must be an error")
	}
	// The output of a describe that forgot --format=json. The failure has to name
	// that, because the alternative is a decode error far from the call.
	_, err := Fact("name: dev\nstatus: RUNNING")
	if err == nil {
		t.Fatal("non-JSON output must be an error")
	}
	if !strings.Contains(err.Error(), "--format=json") {
		t.Fatalf("the error does not name the likely cause: %v", err)
	}
}
