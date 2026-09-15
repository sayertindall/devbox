package machine

import (
	"reflect"
	"strings"
	"testing"

	"devbox/internal/record"
)

// labeledDiskJSON is a data disk devbox claimed for box1, which is the only kind
// destroy may delete.
const labeledDiskJSON = `{"name":"devbox-data","labels":{"devbox-managed":"true","devbox-name":"box1"}}`

// unlabeledDiskJSON is a disk of the same name that carries no devbox labels.
const unlabeledDiskJSON = `{"name":"devbox-data","labels":{"team":"proto"}}`

// imagesJSON is a machine image of box1 and one devbox must not touch. A machine
// image stores the source instance's properties, so those labels are the only
// proof of ownership there is.
const imagesJSON = `[{"name":"box1-20260915-120000","instanceProperties":{"labels":{"devbox-managed":"true","devbox-name":"box1"}}},{"name":"proto-image","instanceProperties":{"labels":{"team":"proto"}}}]`

// destroyProbe answers the reads destroy performs before it prints its inventory.
func destroyProbe(t *testing.T, disk string) *probe {
	t.Helper()
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		switch {
		case isVerb(args, "describe"):
			return instanceJSON("box1", "RUNNING"), nil
		case len(args) > 2 && args[1] == "disks" && args[2] == "describe":
			return disk, nil
		case len(args) > 2 && args[1] == "machine-images" && args[2] == "list":
			return imagesJSON, nil
		}
		return "", nil
	}
	return p
}

// TestDestroyWithoutConfirmDeletesNothing is the safety property of the whole
// command: the inventory is printed, and not one delete call is made.
func TestDestroyWithoutConfirmDeletesNothing(t *testing.T) {
	p := destroyProbe(t, labeledDiskJSON)
	err := p.run(t, "destroy", "box1")
	if err == nil || !strings.Contains(err.Error(), "--confirm=box1") {
		t.Fatalf("want a confirmation refusal, got %v", err)
	}
	if hasCall(p.cloud, "instances", "delete") || hasCall(p.cloud, "disks", "delete") || hasCall(p.cloud, "machine-images", "delete") {
		t.Fatalf("destroy without --confirm deleted something:\n%s", p.cloud.Argv())
	}
	out := p.out.String()
	for _, want := range []string{
		"instance box1 (RUNNING) in us-central1-a",
		"data disk devbox-data",
		"machine image box1-20260915-120000 will be kept without --with-images",
		"snapshots of box1 are never deleted by destroy",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("inventory is missing %q:\n%s", want, out)
		}
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused destroy wrote records: %+v", entries)
	}
}

// TestDestroyDeletesTheBoxItsLabeledDiskAndItsImages asserts every delete vector
// and that an image devbox cannot prove it made is left alone.
func TestDestroyDeletesTheBoxItsLabeledDiskAndItsImages(t *testing.T) {
	p := destroyProbe(t, labeledDiskJSON)
	if err := p.run(t, "destroy", "box1", "--confirm=box1", "--with-images"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	wantInstance := []string{"compute", "instances", "delete", "box1", "--project=devbox-probe", "--zone=us-central1-a", "--quiet"}
	if got := call(t, p.cloud, "instances", "delete"); !reflect.DeepEqual(got, wantInstance) {
		t.Fatalf("instance delete argv\n got %#v\nwant %#v", got, wantInstance)
	}
	wantDisk := []string{"compute", "disks", "delete", "devbox-data", "--project=devbox-probe", "--zone=us-central1-a", "--quiet"}
	if got := call(t, p.cloud, "disks", "delete"); !reflect.DeepEqual(got, wantDisk) {
		t.Fatalf("disk delete argv\n got %#v\nwant %#v", got, wantDisk)
	}
	wantImage := []string{"compute", "machine-images", "delete", "box1-20260915-120000", "--project=devbox-probe", "--quiet"}
	if got := call(t, p.cloud, "machine-images", "delete"); !reflect.DeepEqual(got, wantImage) {
		t.Fatalf("image delete argv\n got %#v\nwant %#v", got, wantImage)
	}
	for _, recorded := range p.cloud.Calls() {
		line := strings.Join(recorded, " ")
		if strings.Contains(recorded[2], "delete") && strings.Contains(line, "proto-image") {
			t.Fatalf("destroy deleted an image it did not label: %s", line)
		}
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want one delete record per resource, got %+v", entries)
	}
	for _, entry := range entries {
		if entry.Kind != record.KindDelete || entry.State != record.StateKnown {
			t.Fatalf("record = %+v", entry)
		}
	}
}

// TestDestroyKeepsImagesWithoutTheFlag covers the default: the instance goes, its
// machine images stay until the operator asks for them.
func TestDestroyKeepsImagesWithoutTheFlag(t *testing.T) {
	p := destroyProbe(t, labeledDiskJSON)
	if err := p.run(t, "destroy", "box1", "--confirm=box1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if hasCall(p.cloud, "machine-images", "delete") {
		t.Fatalf("destroy deleted images without --with-images:\n%s", p.cloud.Argv())
	}
	if !strings.Contains(p.out.String(), "will be kept without --with-images") {
		t.Fatalf("inventory does not say the images are kept: %s", p.out.String())
	}
}

// TestDestroyKeepsAnUnlabeledDisk proves the data disk is only deleted when its
// labels name the box, which is the only claim devbox accepts.
func TestDestroyKeepsAnUnlabeledDisk(t *testing.T) {
	p := destroyProbe(t, unlabeledDiskJSON)
	if err := p.run(t, "destroy", "box1", "--confirm=box1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if hasCall(p.cloud, "disks", "delete") {
		t.Fatalf("destroy deleted a disk that is not labeled for the box:\n%s", p.cloud.Argv())
	}
	if !strings.Contains(p.out.String(), "data disk devbox-data is not labeled for box1 and will be kept") {
		t.Fatalf("inventory does not say the disk is kept: %s", p.out.String())
	}
}

// TestDestroyRefusesAnInstanceDevboxDidNotCreate covers the case that matters
// most: a hand-made virtual machine in the same project is never deleted.
func TestDestroyRefusesAnInstanceDevboxDidNotCreate(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		if isVerb(args, "describe") {
			return `[{"name":"proto-vm","status":"RUNNING","labels":{"team":"proto"}}]`, nil
		}
		return "", nil
	}
	err := p.run(t, "destroy", "proto-vm", "--confirm=proto-vm")
	if err == nil || !strings.Contains(err.Error(), "labels") {
		t.Fatalf("want a labels refusal, got %v", err)
	}
	if len(p.cloud.Calls()) != 1 {
		t.Fatalf("a refused destroy kept reading the project:\n%s", p.cloud.Argv())
	}
}
