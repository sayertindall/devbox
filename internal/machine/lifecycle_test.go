package machine

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"devbox/internal/record"
)

// TestStateVerbsCallTheirVerb runs the four reversible state verbs and asserts
// each argument vector, because a wrong verb here stops or resumes the wrong
// thing without saying so.
func TestStateVerbsCallTheirVerb(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING", "box1")
	for _, verb := range []string{"start", "stop", "suspend", "resume"} {
		if err := p.run(t, verb, "box1"); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		want := []string{"compute", "instances", verb, "box1", "--project=devbox-probe", "--zone=us-central1-a", "--quiet"}
		if got := call(t, p.cloud, "instances", verb); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s argv\n got %#v\nwant %#v", verb, got, want)
		}
	}
}

// TestStopRefusesAnInstanceDevboxDidNotCreate proves a bare virtual machine in
// the same project is left alone, labels being the only proof of ownership.
func TestStopRefusesAnInstanceDevboxDidNotCreate(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		if isVerb(args, "describe") {
			return `[{"name":"proto-vm","status":"RUNNING","labels":{"team":"proto"},"networkInterfaces":[{"networkIP":"10.128.0.9"}]}]`, nil
		}
		return "", nil
	}
	err := p.run(t, "stop", "proto-vm")
	if err == nil || !strings.Contains(err.Error(), "labels") {
		t.Fatalf("want a labels refusal, got %v", err)
	}
	if hasCall(p.cloud, "instances", "stop") {
		t.Fatalf("stop ran on a foreign instance:\n%s", p.cloud.Argv())
	}
}

// TestListShowsOnlyBoxesDevboxCreated proves the project listing is filtered by
// the same ownership labels every other verb uses.
func TestListShowsOnlyBoxesDevboxCreated(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		if len(args) > 2 && args[1] == "instances" && args[2] == "list" {
			mine := instanceJSON("box1", "RUNNING")
			foreign := `{"name":"proto-vm","zone":"https://www.googleapis.com/compute/v1/projects/devbox-probe/zones/us-central1-a","machineType":"https://www.googleapis.com/compute/v1/projects/devbox-probe/zones/us-central1-a/machineTypes/n2-standard-4","status":"TERMINATED","labels":{"team":"proto"},"networkInterfaces":[{"networkIP":"10.128.0.9"}]}`
			return "[" + mine + "," + foreign + "]", nil
		}
		return "", nil
	}
	if err := p.run(t, "list"); err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"compute", "instances", "list", "--project=devbox-probe", "--format=json"}
	got := call(t, p.cloud, "instances", "list")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list argv\n got %#v\nwant %#v", got, want)
	}
	// The columns are aligned with a tabwriter, so the assertion is on what the
	// table reports rather than on how wide the padding happens to be.
	out := p.out.String()
	header, row := "", ""
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) > 0 && fields[0] == "NAME":
			header = strings.Join(fields, " ")
		case len(fields) > 0 && fields[0] == "box1":
			row = strings.Join(fields, " ")
		}
	}
	if header != "NAME ZONE STATUS MACHINE ADDRESS" {
		t.Fatalf("list header is missing a column: %q", header)
	}
	if row != "box1 us-central1-a RUNNING n2-standard-16 10.128.0.2" {
		t.Fatalf("list does not report the box and its address: %q", row)
	}
	if strings.Contains(out, "proto-vm") {
		t.Fatalf("list reported an instance devbox did not create: %s", out)
	}
}

// TestListReportsNoBoxesWhenNothingIsLabeled covers the empty project: the
// command says so instead of printing a bare header.
func TestListReportsNoBoxesWhenNothingIsLabeled(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func([]string) (string, error) { return "[]", nil }
	if err := p.run(t, "list"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(p.out.String(), "no boxes") {
		t.Fatalf("list output = %s", p.out.String())
	}
}

// TestShowPrintsTheFactsAndTheBlockingRecords covers what an operator needs when
// a box misbehaves: where it is, how to reach it, which disk a snapshot would
// capture, and whether an unfinished operation holds it back.
func TestShowPrintsTheFactsAndTheBlockingRecords(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		if isVerb(args, "describe") {
			return instanceJSON("box1", "RUNNING"), nil
		}
		return "", nil
	}
	if err := p.run(t, "show", "box1"); err != nil {
		t.Fatalf("show: %v", err)
	}
	out := p.out.String()
	for _, want := range []string{
		"name box1",
		"zone us-central1-a",
		"status RUNNING",
		"machine n2-standard-16",
		"internal 10.128.0.2",
		"external none",
		"data disk devbox-data",
		"records none unresolved",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output is missing %q:\n%s", want, out)
		}
	}
	entry, err := p.records.Begin(record.KindDelete, "box1", []string{"compute", "instances", "delete", "box1"})
	if err != nil {
		t.Fatalf("begin record: %v", err)
	}
	p.out.Reset()
	if err := p.run(t, "show", "box1"); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(p.out.String(), "records 1 unresolved") || !strings.Contains(p.out.String(), entry.ID) {
		t.Fatalf("show does not report the blocking record: %s", p.out.String())
	}
}

// TestScheduleCreatesAndAttachesThePolicy asserts both calls and their order, so
// a schedule is never attached before the policy it names exists.
func TestScheduleCreatesAndAttachesThePolicy(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		if isVerb(args, "describe") {
			return instanceJSON("box1", "RUNNING"), nil
		}
		if len(args) > 2 && args[1] == "resource-policies" && args[2] == "describe" {
			return "", missing("resource policy")
		}
		return "", nil
	}
	if err := p.run(t, "schedule", "box1", "--start=09:00", "--stop=19:00", "--timezone=America/Denver"); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	wantCreate := []string{
		"compute", "resource-policies", "create", "instance-schedule", "devbox-box1-schedule",
		"--project=devbox-probe", "--region=us-central1",
		"--vm-start-schedule=09:00", "--vm-stop-schedule=19:00", "--timezone=America/Denver",
	}
	if got := call(t, p.cloud, "resource-policies", "create"); !reflect.DeepEqual(got, wantCreate) {
		t.Fatalf("policy create argv\n got %#v\nwant %#v", got, wantCreate)
	}
	wantAttach := []string{
		"compute", "instances", "add-resource-policies", "box1",
		"--project=devbox-probe", "--zone=us-central1-a", "--resource-policies=devbox-box1-schedule",
	}
	if got := call(t, p.cloud, "instances", "add-resource-policies"); !reflect.DeepEqual(got, wantAttach) {
		t.Fatalf("attach argv\n got %#v\nwant %#v", got, wantAttach)
	}
	if position(t, p.cloud, "resource-policies describe") > position(t, p.cloud, "instance-schedule") {
		t.Fatalf("the policy was created before it was checked:\n%s", p.cloud.Argv())
	}
}

// TestScheduleIsIdempotent covers a policy that is already there: the box is
// attached again and nothing is created a second time.
func TestScheduleIsIdempotent(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		if isVerb(args, "describe") {
			return instanceJSON("box1", "RUNNING"), nil
		}
		return "{}", nil
	}
	if err := p.run(t, "schedule", "box1", "--start=09:00", "--stop=19:00"); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if hasCall(p.cloud, "resource-policies", "create") {
		t.Fatalf("an existing policy was created again:\n%s", p.cloud.Argv())
	}
	if !hasCall(p.cloud, "instances", "add-resource-policies") {
		t.Fatalf("the existing policy was not attached:\n%s", p.cloud.Argv())
	}
	if !strings.Contains(p.out.String(), "already exists") {
		t.Fatalf("output = %s", p.out.String())
	}
}

// TestScheduleRemovesTheWindows asserts the detach happens before the policy is
// deleted, because a policy still attached to a box cannot be removed.
func TestScheduleRemovesTheWindows(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING", "box1")
	if err := p.run(t, "schedule", "box1", "--remove"); err != nil {
		t.Fatalf("schedule --remove: %v", err)
	}
	wantDetach := []string{
		"compute", "instances", "remove-resource-policies", "box1",
		"--project=devbox-probe", "--zone=us-central1-a", "--resource-policies=devbox-box1-schedule",
	}
	if got := call(t, p.cloud, "instances", "remove-resource-policies"); !reflect.DeepEqual(got, wantDetach) {
		t.Fatalf("detach argv\n got %#v\nwant %#v", got, wantDetach)
	}
	wantDelete := []string{"compute", "resource-policies", "delete", "devbox-box1-schedule", "--project=devbox-probe", "--region=us-central1"}
	if got := call(t, p.cloud, "resource-policies", "delete"); !reflect.DeepEqual(got, wantDelete) {
		t.Fatalf("policy delete argv\n got %#v\nwant %#v", got, wantDelete)
	}
	if position(t, p.cloud, "remove-resource-policies") > position(t, p.cloud, "resource-policies delete") {
		t.Fatalf("the policy was deleted before it was detached:\n%s", p.cloud.Argv())
	}
}

// TestScheduleRefusesWithoutWindows covers the half-specified schedule, which
// would otherwise create a policy that never stops the box.
func TestScheduleRefusesWithoutWindows(t *testing.T) {
	p := newProbe(t)
	p.describeFor("RUNNING", "box1")
	err := p.run(t, "schedule", "box1", "--start=09:00")
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("want a usage refusal, got %v", err)
	}
	if hasCall(p.cloud, "resource-policies", "create") {
		t.Fatalf("a half-specified schedule created a policy:\n%s", p.cloud.Argv())
	}
}

func TestNewTellsTheOperatorWhatComesNext(t *testing.T) {
	p := newProbe(t)
	p.cloud.Reply = func(args []string) (string, error) {
		if len(args) > 2 && args[1] == "instances" && args[2] == "describe" {
			return "", errors.New("ERROR: The resource was not found")
		}
		return "[]", nil
	}
	if err := p.run(t, "new", "box1"); err != nil {
		t.Fatalf("new: %v", err)
	}
	out := p.out.String()
	// A box with no external address needs NAT before it can reach anything, and
	// the operator should not have to read the source to learn that.
	if !strings.Contains(out, "devbox network ensure") {
		t.Fatalf("new does not mention the network step:\n%s", out)
	}
	if !strings.Contains(out, "devbox ssh box1") {
		t.Fatalf("new does not say how to reach the box:\n%s", out)
	}
}
