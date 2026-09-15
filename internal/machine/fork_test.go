package machine

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"devbox/internal/record"
)

// imageName is the shape of a machine image name: the source box, then the UTC
// moment, so a fork can always be traced back to what it copied.
var imageName = regexp.MustCompile(`^source-\d{8}-\d{6}$`)

// TestForkCapturesTheSourceThenCreatesTheNewBox asserts both calls in order: a
// machine image of the whole source, then an instance created from that image
// with the labels and bounds of the new box.
func TestForkCapturesTheSourceThenCreatesTheNewBox(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING", "source")
	if err := p.run(t, "fork", "source", "target"); err != nil {
		t.Fatalf("fork: %v", err)
	}
	image := call(t, p.cloud, "machine-images", "create")
	if !imageName.MatchString(image[3]) {
		t.Fatalf("machine image name = %q", image[3])
	}
	wantImage := []string{
		"compute", "machine-images", "create", image[3],
		"--project=devbox-probe",
		"--source-instance=source",
		"--source-instance-zone=us-central1-a",
	}
	if !reflect.DeepEqual(image, wantImage) {
		t.Fatalf("machine image argv\n got %#v\nwant %#v", image, wantImage)
	}
	wantCreate := []string{
		"compute", "instances", "create", "target",
		"--project=devbox-probe", "--zone=us-central1-a",
		"--source-machine-image=" + image[3],
		"--no-address",
		"--service-account=devbox@devbox-probe.iam.gserviceaccount.com", "--scopes=cloud-platform",
		"--metadata=enable-oslogin=TRUE,startup-script-url=gs://devbox-bootstrap/startup.sh",
		"--labels=devbox-managed=true,devbox-name=target",
		"--tags=devbox",
		"--max-run-duration=12h", "--instance-termination-action=STOP",
	}
	if got := call(t, p.cloud, "instances", "create"); !reflect.DeepEqual(got, wantCreate) {
		t.Fatalf("fork create argv\n got %#v\nwant %#v", got, wantCreate)
	}
	if position(t, p.cloud, "machine-images create") > position(t, p.cloud, "instances create") {
		t.Fatalf("the new box was created before the image it copies:\n%s", p.cloud.Argv())
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want a record for the image, the new box, and its disk label, got %+v", entries)
	}
	if entries[0].Kind != record.KindFork || entries[0].Box != "source" || entries[0].Result != "machine image "+image[3] {
		t.Fatalf("image record = %+v", entries[0])
	}
	if entries[1].Kind != record.KindFork || entries[1].Box != "target" || entries[1].Result != "instance target" {
		t.Fatalf("box record = %+v", entries[1])
	}
	if strings.Contains(p.errOut.String(), "unresolved") {
		t.Fatalf("a successful fork reported an unresolved record: %s", p.errOut.String())
	}
}

// TestForkRefusesANameThatIsTaken proves the new name is checked before the
// machine image is taken, so a refused fork costs nothing.
func TestForkRefusesANameThatIsTaken(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING", "source", "target")
	err := p.run(t, "fork", "source", "target")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want an already exists refusal, got %v", err)
	}
	if hasCall(p.cloud, "machine-images", "create") || hasCall(p.cloud, "instances", "create") {
		t.Fatalf("a refused fork still spent resources:\n%s", p.cloud.Argv())
	}
}

// TestForkRefusesWhileTheSourceHasUnresolvedRecords proves a source whose last
// operation never concluded is not copied, and that the refusal names the record.
func TestForkRefusesWhileTheSourceHasUnresolvedRecords(t *testing.T) {
	p := newProbe(t)
	entry, err := p.records.Begin(record.KindSnapshot, "source", []string{"compute", "snapshots", "create", "source-20260915-120000"})
	if err != nil {
		t.Fatalf("begin record: %v", err)
	}
	err = p.run(t, "fork", "source", "target")
	if err == nil || !strings.Contains(err.Error(), entry.ID) {
		t.Fatalf("want the source's unresolved record to block the fork, got %v", err)
	}
	if len(p.cloud.Calls()) != 0 {
		t.Fatalf("a blocked fork still called the cloud:\n%s", p.cloud.Argv())
	}
}

// TestForkRefusesItsOwnSource covers the name check that keeps a fork from
// capturing a box onto itself.
func TestForkRefusesItsOwnSource(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING", "source")
	err := p.run(t, "fork", "source", "source")
	if err == nil || !strings.Contains(err.Error(), "cannot be forked onto itself") {
		t.Fatalf("want a self-fork refusal, got %v", err)
	}
	if len(p.cloud.Calls()) != 0 {
		t.Fatalf("a refused fork still called the cloud:\n%s", p.cloud.Argv())
	}
}

// TestForkKeepsTheImageWhenTheNewBoxFails proves the operator is told the image
// survived a failed create, because that is the expensive half of a fork.
func TestForkKeepsTheImageWhenTheNewBoxFails(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		switch {
		case isVerb(args, "create"):
			return "", missing("machine type n9-standard-99")
		case isVerb(args, "describe"):
			if args[3] == "source" {
				return instanceJSON("source", "RUNNING"), nil
			}
			return "", missing("instance")
		}
		return "", nil
	}
	if err := p.run(t, "fork", "source", "target"); err == nil {
		t.Fatal("want the create failure to surface")
	}
	if !strings.Contains(p.errOut.String(), "machine image source-") {
		t.Fatalf("stderr does not name the surviving image: %s", p.errOut.String())
	}
	if !strings.Contains(p.errOut.String(), "retry the new box by hand") {
		t.Fatalf("stderr does not show how to finish the fork: %s", p.errOut.String())
	}
}
