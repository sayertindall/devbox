package machine

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/record"
)

// snapshotName is the shape of a derived snapshot name: the box, then the UTC
// moment, so two snapshots of one box never collide and sort by age.
var snapshotName = regexp.MustCompile(`^box1-\d{8}-\d{6}$`)

// TestSnapshotStopsTheStorageServicesAroundTheCapture asserts the order of the
// whole operation, including that a failed snapshot still leaves the box with
// docker and containerd running: a crash-consistent capture of a torn image layer
// is worse than no snapshot, and a box left without its storage services is an
// outage.
func TestSnapshotStopsTheStorageServicesAroundTheCapture(t *testing.T) {
	p := newProbe(t)
	var order []string
	session := &access.Recording{Reply: func(command string) (string, error) {
		order = append(order, "remote: "+command)
		return "", nil
	}}
	withSession(t, session)
	p.cloud.Reply = func(args []string) (string, error) {
		switch {
		case isVerb(args, "describe"):
			return instanceJSON(args[3], "RUNNING"), nil
		case len(args) > 2 && args[1] == "snapshots" && args[2] == "create":
			order = append(order, "cloud: "+strings.Join(args, " "))
			return "", errors.New("gcloud compute snapshots create: QUOTA_EXCEEDED")
		}
		return "", nil
	}
	err := p.run(t, "snapshot", "box1")
	if err == nil {
		t.Fatal("want the snapshot failure to surface")
	}
	if len(order) != 4 {
		t.Fatalf("expected a stop, a flush, the capture and a start, got %q", order)
	}
	if order[0] != "remote: "+quiesceStop {
		t.Fatalf("first step = %q, want the storage services stopped", order[0])
	}
	if order[1] != "remote: "+quiesceFlush {
		t.Fatalf("second step = %q, want the writes flushed to the disk", order[1])
	}
	if !strings.HasPrefix(order[2], "cloud: compute snapshots create box1-") {
		t.Fatalf("third step = %q, want the snapshot of the stopped disk", order[2])
	}
	if order[3] != "remote: "+quiesceStart {
		t.Fatalf("the services were not restarted after the failure: %q", order)
	}

	got := call(t, p.cloud, "snapshots", "create")
	if !snapshotName.MatchString(got[3]) {
		t.Fatalf("snapshot name = %q", got[3])
	}
	want := []string{"compute", "snapshots", "create", got[3], "--project=devbox-probe", "--source-disk=devbox-data", "--source-disk-zone=us-central1-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot argv\n got %#v\nwant %#v", got, want)
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != record.KindSnapshot || entries[0].State != record.StateUnknown {
		t.Fatalf("records = %+v, want one unresolved snapshot record", entries)
	}
	if !strings.Contains(p.errOut.String(), "snapshots create "+got[3]) {
		t.Fatalf("stderr does not show what to check: %s", p.errOut.String())
	}
}

// TestSnapshotPrintsTheName covers the successful capture, which is the only
// handle the operator has on the snapshot afterwards.
func TestSnapshotPrintsTheName(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING", "box1")
	session := &access.Recording{}
	withSession(t, session)
	if err := p.run(t, "snapshot", "box1"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	name := strings.TrimPrefix(strings.TrimSpace(p.out.String()), "snapshot ")
	if !snapshotName.MatchString(name) {
		t.Fatalf("printed %q, want the snapshot name", p.out.String())
	}
	if !reflect.DeepEqual(session.Commands, []string{quiesceStop, quiesceFlush, quiesceStart}) {
		t.Fatalf("remote commands = %q", session.Commands)
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 1 || entries[0].State != record.StateKnown || entries[0].Result != "snapshot "+name {
		t.Fatalf("records = %+v", entries)
	}
}

// TestSnapshotRefusesAStoppedBox proves the command does not try to stop services
// on a box that is not running, and says why.
func TestSnapshotRefusesAStoppedBox(t *testing.T) {
	p := newProbe(t)
	p.describeFor("TERMINATED", "box1")
	session := &access.Recording{}
	withSession(t, session)
	err := p.run(t, "snapshot", "box1")
	if err == nil || !strings.Contains(err.Error(), "start it before snapshotting") {
		t.Fatalf("want a stopped-box refusal, got %v", err)
	}
	if len(session.Commands) != 0 {
		t.Fatalf("a stopped box was touched over ssh: %q", session.Commands)
	}
	if hasCall(p.cloud, "snapshots", "create") {
		t.Fatalf("a stopped box was snapshotted:\n%s", p.cloud.Argv())
	}
}

// TestSnapshotDryRunLeavesTheBoxAlone proves a rehearsal reads nothing, touches
// nothing, and still shows the operator the exact capture it would take.
func TestSnapshotDryRunLeavesTheBoxAlone(t *testing.T) {
	p := newProbe(t)
	p.deps.DryRun = true
	p.describeFor("RUNNING", "box1")
	session := &access.Recording{}
	withSession(t, session)
	if err := p.run(t, "snapshot", "box1"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(session.Commands) != 0 {
		t.Fatalf("a dry run touched the box: %q", session.Commands)
	}
	// The rehearsal prints the call rather than making it, so there is nothing to
	// find in the recorded cloud calls; what matters is what the operator read.
	out := p.out.String()
	if !strings.Contains(out, "gcloud compute snapshots create box1") {
		t.Fatalf("the rehearsal did not show the snapshot call:\n%s", out)
	}
	if !strings.Contains(out, "systemctl stop docker containerd") {
		t.Fatalf("the rehearsal did not show the quiesce it would perform:\n%s", out)
	}
	if len(p.cloud.Calls()) != 0 {
		t.Fatalf("a rehearsal must not call the cloud:\n%s", p.cloud.Argv())
	}
	entries, err := p.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a dry run wrote records: %+v", entries)
	}
}

// TestSnapshotFailsWhenTheSessionCannotBeOpened covers the transport refusal: the
// error must reach the operator rather than being reported as a snapshot.
func TestSnapshotFailsWhenTheSessionCannotBeOpened(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING", "box1")
	previous := openSession
	openSession = func(context.Context, cli.Deps, box.Name) (access.Session, error) {
		return nil, errors.New("ssh: no route to host")
	}
	t.Cleanup(func() { openSession = previous })
	if err := p.run(t, "snapshot", "box1"); err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Fatalf("want the transport failure, got %v", err)
	}
}

// TestSnapshotReportsAServiceThatCannotBeStopped proves a box whose services
// refuse to stop is not captured anyway, since the capture would be torn.
func TestSnapshotReportsAServiceThatCannotBeStopped(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING", "box1")
	session := &access.Recording{Fail: "systemctl stop"}
	withSession(t, session)
	err := p.run(t, "snapshot", "box1")
	if err == nil || !strings.Contains(err.Error(), "stop docker and containerd") {
		t.Fatalf("want a stop failure, got %v", err)
	}
	if hasCall(p.cloud, "snapshots", "create") {
		t.Fatalf("the disk was captured with its services running:\n%s", p.cloud.Argv())
	}
}
