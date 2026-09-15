package machine

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"devbox/internal/record"
)

// TestNewBuildsTheConfiguredCreateCall asserts the whole argument vector, flag by
// flag, because this is the call that spends the money and every flag in it comes
// from a product decision.
func TestNewBuildsTheConfiguredCreateCall(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING")
	if err := p.run(t, "new", "box1"); err != nil {
		t.Fatalf("new: %v", err)
	}
	want := []string{
		"compute", "instances", "create", "box1",
		"--project=devbox-probe", "--zone=us-central1-a",
		"--machine-type=n2-standard-16",
		"--image-project=debian-cloud", "--image-family=debian-13",
		"--boot-disk-size=100GB", "--boot-disk-type=pd-balanced",
		"--create-disk=name=devbox-data,size=1024GB,type=pd-ssd,device-name=devbox-data,auto-delete=no",
		"--no-address",
		"--service-account=devbox@devbox-probe.iam.gserviceaccount.com", "--scopes=cloud-platform",
		"--metadata=enable-oslogin=TRUE,startup-script-url=gs://devbox-bootstrap/startup.sh",
		"--labels=devbox-managed=true,devbox-name=box1",
		"--tags=devbox",
		"--max-run-duration=12h", "--instance-termination-action=STOP",
	}
	got := call(t, p.cloud, "instances", "create")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("create argv\n got %#v\nwant %#v", got, want)
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want a record for the create and one for the disk label, got %d", len(entries))
	}
	if entries[0].Kind != record.KindCreate || entries[0].Box != "box1" || entries[0].State != record.StateKnown {
		t.Fatalf("create record = %+v", entries[0])
	}
	if entries[0].Result != "instance box1" {
		t.Fatalf("create record result = %q", entries[0].Result)
	}
	if !reflect.DeepEqual(entries[0].Args, want) {
		t.Fatalf("recorded argv %#v does not match the call %#v", entries[0].Args, want)
	}
}

// TestNewLabelsTheDataDisk asserts that the disk a box is created with is
// claimed for that box, because destroy may only delete a disk whose labels name
// the box.
func TestNewLabelsTheDataDisk(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING")
	if err := p.run(t, "new", "box1"); err != nil {
		t.Fatalf("new: %v", err)
	}
	want := []string{
		"compute", "disks", "update", "devbox-data",
		"--project=devbox-probe", "--zone=us-central1-a",
		"--update-labels=devbox-managed=true,devbox-name=box1",
	}
	got := call(t, p.cloud, "disks", "update")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("disk update argv\n got %#v\nwant %#v", got, want)
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 2 || entries[1].Result != "data disk devbox-data labeled for box1" {
		t.Fatalf("records = %+v", entries)
	}
}

// TestNewRefusesANameThatIsTaken proves the refusal costs nothing: the name is
// checked with a describe and no create call is made.
func TestNewRefusesANameThatIsTaken(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING", "box1")
	err := p.run(t, "new", "box1")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want an already exists refusal, got %v", err)
	}
	if hasCall(p.cloud, "instances", "create") {
		t.Fatalf("new created an instance anyway:\n%s", p.cloud.Argv())
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused create wrote records: %+v", entries)
	}
}

// TestNewRefusesWhileARecordIsUnresolved proves the block happens before any
// cloud call, and that the operator is told which record holds the box.
func TestNewRefusesWhileARecordIsUnresolved(t *testing.T) {
	p := newProbe(t)
	entry, err := p.records.Begin(record.KindFork, "box1", []string{"compute", "machine-images", "create", "box1-20260915-120000"})
	if err != nil {
		t.Fatalf("begin record: %v", err)
	}
	err = p.run(t, "new", "box1")
	if err == nil {
		t.Fatal("want the unresolved record to block the create")
	}
	if !strings.Contains(err.Error(), entry.ID) {
		t.Fatalf("refusal does not name the blocking record: %v", err)
	}
	if !strings.Contains(err.Error(), "compute machine-images create") {
		t.Fatalf("refusal does not show what to check: %v", err)
	}
	if len(p.cloud.Calls()) != 0 {
		t.Fatalf("a blocked create still called the cloud:\n%s", p.cloud.Argv())
	}
}

// TestNewRefusesWithoutABootstrapScript proves a box cannot be created that would
// boot with no toolchain, unless the operator says that is what they want.
func TestNewRefusesWithoutABootstrapScript(t *testing.T) {
	p := newProbe(t)
	p.deps.Config.BootstrapURL = ""
	p.describeFor("RUNNING")
	err := p.run(t, "new", "box1")
	if err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("want a bootstrap refusal, got %v", err)
	}
	if len(p.cloud.Calls()) != 0 {
		t.Fatalf("a refused create still called the cloud:\n%s", p.cloud.Argv())
	}
}

// TestNewWithoutBootstrapDropsTheStartupScript checks the override keeps OS Login
// on: a box with no startup script must still be reachable.
func TestNewWithoutBootstrapDropsTheStartupScript(t *testing.T) {
	p := newProbe(t)
	p.deps.Config.BootstrapURL = ""
	p.describeFor("RUNNING")
	if err := p.run(t, "new", "box1", "--no-bootstrap"); err != nil {
		t.Fatalf("new --no-bootstrap: %v", err)
	}
	metadata := flagValue(call(t, p.cloud, "instances", "create"), "--metadata")
	if metadata != "enable-oslogin=TRUE" {
		t.Fatalf("metadata of a bare box = %q, want os login only", metadata)
	}
}

// TestNewAttachesTheSchedulePolicy covers the one flag new adds for scheduling,
// and that the policy name is the one the schedule verb creates.
func TestNewAttachesTheSchedulePolicy(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING")
	if err := p.run(t, "new", "box1", "--schedule"); err != nil {
		t.Fatalf("new --schedule: %v", err)
	}
	policy := flagValue(call(t, p.cloud, "instances", "create"), "--resource-policies")
	if policy != schedulePolicy("box1") {
		t.Fatalf("resource policy = %q, want %q", policy, schedulePolicy("box1"))
	}
}

// TestNewRecordsAnAmbiguousFailureAsUnresolved proves a call whose result devbox
// cannot interpret stays visible and blocks the next mutation, with the exact
// command to reconcile it printed.
func TestNewRecordsAnAmbiguousFailureAsUnresolved(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		if isVerb(args, "describe") {
			return "", missing("instance")
		}
		if isVerb(args, "create") {
			return "", errors.New("gcloud compute instances create box1: QUOTA_EXCEEDED")
		}
		return "", nil
	}
	if err := p.run(t, "new", "box1"); err == nil {
		t.Fatal("want the create failure to surface")
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 1 || entries[0].State != record.StateUnknown {
		t.Fatalf("records = %+v, want one unresolved record", entries)
	}
	if !strings.Contains(p.errOut.String(), "record "+entries[0].ID+" is unresolved") {
		t.Fatalf("stderr does not name the unresolved record: %s", p.errOut.String())
	}
	if !strings.Contains(p.errOut.String(), "compute instances create box1") {
		t.Fatalf("stderr does not print the command to check: %s", p.errOut.String())
	}
	if err := p.run(t, "new", "box1"); err == nil {
		t.Fatal("want the unresolved record to block the next create")
	}
}

// TestNewFreesTheNameWhenNothingWasCreated proves a failure that provably created
// nothing does not block the next attempt.
func TestNewFreesTheNameWhenNothingWasCreated(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		if isVerb(args, "describe") {
			return "", missing("instance")
		}
		if isVerb(args, "create") {
			return "", missing("image family debian-13")
		}
		return "", nil
	}
	if err := p.run(t, "new", "box1"); err == nil {
		t.Fatal("want the create failure to surface")
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 1 || entries[0].State != record.StateKnown || entries[0].Result != "" {
		t.Fatalf("records = %+v, want one settled failure", entries)
	}
	unresolved, err := p.records.Unresolved("box1")
	if err != nil {
		t.Fatalf("read unresolved: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("a provably empty create still blocks the box: %+v", unresolved)
	}
}
