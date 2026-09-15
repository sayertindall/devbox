package network

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
)

// probe is one test's dependencies: a configuration and a recorded cloud, so no
// call leaves the machine.
type probe struct {
	deps  cli.Deps
	cloud *gcloud.Fake
	out   *bytes.Buffer
}

func newProbe(t *testing.T, reply func(args []string) (string, error)) *probe {
	t.Helper()
	cfg := config.Default()
	cfg.Project = "devbox-probe"
	out := &bytes.Buffer{}
	cloud := &gcloud.Fake{Reply: reply}
	return &probe{
		deps:  cli.Deps{Config: cfg, Cloud: cloud, Out: out, Err: &bytes.Buffer{}},
		cloud: cloud,
		out:   out,
	}
}

// run dispatches a network verb through the registered command, so a test
// exercises the same path the binary does.
func (p *probe) run(t *testing.T, args ...string) error {
	t.Helper()
	commands := Commands()
	if len(commands) != 1 {
		t.Fatalf("want one network command, got %d", len(commands))
	}
	return commands[0].Run(context.Background(), p.deps, args)
}

// absent answers a describe with the error gcloud reports for a resource that is
// not there, and lets a create succeed.
func absent(args []string) (string, error) {
	for _, arg := range args {
		if arg == "describe" {
			return "", errors.New("the resource was not found")
		}
	}
	return "", nil
}

// call finds the first recorded call whose words after "compute" match the given
// sequence, which a test needs because the NAT has both a describe and a create
// under the same two leading words.
func call(t *testing.T, cloud *gcloud.Fake, words ...string) []string {
	t.Helper()
	for _, recorded := range cloud.Calls() {
		if len(recorded) >= 1+len(words) && reflect.DeepEqual(recorded[1:1+len(words)], words) {
			return recorded
		}
	}
	t.Fatalf("no call %q was made; calls:\n%s", words, cloud.Argv())
	return nil
}

// position is where a call carrying every fragment happened, so a test can prove
// an object was checked before it was created.
func position(t *testing.T, cloud *gcloud.Fake, fragments ...string) int {
	t.Helper()
	for index, recorded := range cloud.Calls() {
		line := strings.Join(recorded, " ")
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(line, fragment) {
				matched = false
				break
			}
		}
		if matched {
			return index
		}
	}
	t.Fatalf("no call with %q was made; calls:\n%s", fragments, cloud.Argv())
	return -1
}

// TestEnsureCreatesWhatIsMissing asserts every create vector and that each object
// is described first, which is what makes the command safe to repeat.
func TestEnsureCreatesWhatIsMissing(t *testing.T) {
	p := newProbe(t, absent)
	if err := p.run(t, "ensure"); err != nil {
		t.Fatalf("network ensure: %v", err)
	}
	wantFirewall := []string{
		"compute", "firewall-rules", "create", "devbox-ssh",
		"--project=devbox-probe",
		"--network=default",
		"--allow=tcp:22",
		"--source-ranges=35.235.240.0/20",
		"--target-tags=devbox",
		"--description=SSH through IAP for devbox boxes",
	}
	if got := call(t, p.cloud, "firewall-rules", "create"); !reflect.DeepEqual(got, wantFirewall) {
		t.Fatalf("firewall create argv\n got %#v\nwant %#v", got, wantFirewall)
	}
	wantRouter := []string{
		"compute", "routers", "create", "devbox-router",
		"--project=devbox-probe", "--region=us-central1",
		"--network=default",
		"--description=egress for devbox boxes without an external address",
	}
	if got := call(t, p.cloud, "routers", "create"); !reflect.DeepEqual(got, wantRouter) {
		t.Fatalf("router create argv\n got %#v\nwant %#v", got, wantRouter)
	}
	wantNat := []string{
		"compute", "routers", "nats", "create", "devbox-nat",
		"--project=devbox-probe", "--region=us-central1", "--router=devbox-router",
		"--auto-allocate-nat-external-ips", "--nat-all-subnet-ip-ranges",
	}
	if got := call(t, p.cloud, "routers", "nats", "create"); !reflect.DeepEqual(got, wantNat) {
		t.Fatalf("nat create argv\n got %#v\nwant %#v", got, wantNat)
	}
	order := [][2]string{
		{"firewall-rules describe", "firewall-rules create"},
		{"firewall-rules create", "routers describe"},
		{"routers describe", "routers create"},
		{"routers create", "nats describe"},
		{"nats describe", "routers nats create"},
	}
	for _, pair := range order {
		if position(t, p.cloud, pair[0]) > position(t, p.cloud, pair[1]) {
			t.Fatalf("%s did not come before %s:\n%s", pair[0], pair[1], p.cloud.Argv())
		}
	}
	out := p.out.String()
	for _, want := range []string{
		"created firewall rule devbox-ssh",
		"created router devbox-router",
		"created nat devbox-nat",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("ensure output is missing %q:\n%s", want, out)
		}
	}
}

// TestEnsureIsIdempotent covers a project that already has the whole path: it
// reports what is there and creates nothing.
func TestEnsureIsIdempotent(t *testing.T) {
	p := newProbe(t, func([]string) (string, error) { return "{}", nil })
	if err := p.run(t, "ensure"); err != nil {
		t.Fatalf("network ensure: %v", err)
	}
	for _, recorded := range p.cloud.Calls() {
		if recorded[2] == "create" {
			t.Fatalf("an existing object was created again: %s", strings.Join(recorded, " "))
		}
	}
	out := p.out.String()
	if strings.Count(out, "already exists") != 3 {
		t.Fatalf("ensure did not report all three objects as present:\n%s", out)
	}
}

// TestEnsureStopsOnAReadItCannotInterpret proves an unreadable project is not
// answered by creating objects on top of whatever is there.
func TestEnsureStopsOnAReadItCannotInterpret(t *testing.T) {
	p := newProbe(t, func([]string) (string, error) { return "", errors.New("PERMISSION_DENIED") })
	err := p.run(t, "ensure")
	if err == nil || !strings.Contains(err.Error(), "PERMISSION_DENIED") {
		t.Fatalf("want the read failure, got %v", err)
	}
	for _, recorded := range p.cloud.Calls() {
		if recorded[2] == "create" {
			t.Fatalf("ensure created something after a failed read: %s", strings.Join(recorded, " "))
		}
	}
}

// TestShowReportsEveryPiece asserts the reads and the reported state, including
// whether Private Google Access is on for the subnet.
func TestShowReportsEveryPiece(t *testing.T) {
	p := newProbe(t, func(args []string) (string, error) {
		switch {
		case args[1] == "firewall-rules":
			return `{"name":"devbox-ssh","sourceRanges":["35.235.240.0/20"],"targetTags":["devbox"],"allowed":[{"IPProtocol":"tcp","ports":["22"]}]}`, nil
		case args[1] == "routers" && args[2] == "describe":
			return `{"name":"devbox-router"}`, nil
		case args[1] == "routers" && args[2] == "nats":
			return `{"name":"devbox-nat","natIpAllocateOption":"AUTO_ONLY","sourceSubnetworkIpRangesToNat":"ALL_SUBNETWORKS_ALL_IP_RANGES"}`, nil
		case args[1] == "networks":
			return `{"name":"default","privateIpGoogleAccess":false}`, nil
		}
		return "", nil
	})
	if err := p.run(t, "show"); err != nil {
		t.Fatalf("network show: %v", err)
	}
	wantFirewall := []string{"compute", "firewall-rules", "describe", "devbox-ssh", "--project=devbox-probe"}
	if got := p.cloud.Calls()[0]; !reflect.DeepEqual(got, wantFirewall) {
		t.Fatalf("firewall describe argv\n got %#v\nwant %#v", got, wantFirewall)
	}
	wantNat := []string{"compute", "routers", "nats", "describe", "devbox-nat", "--project=devbox-probe", "--region=us-central1", "--router=devbox-router"}
	found := false
	for _, recorded := range p.cloud.Calls() {
		if reflect.DeepEqual(recorded, wantNat) {
			found = true
		}
		if recorded[2] == "create" {
			t.Fatalf("show created something: %s", strings.Join(recorded, " "))
		}
	}
	if !found {
		t.Fatalf("show did not describe the nat; calls:\n%s", p.cloud.Argv())
	}
	out := p.out.String()
	for _, want := range []string{
		"firewall devbox-ssh enabled: allow tcp:22 from 35.235.240.0/20 to tag devbox",
		"router devbox-router present in us-central1",
		"nat devbox-nat on router devbox-router: AUTO_ONLY, ALL_SUBNETWORKS_ALL_IP_RANGES",
		"subnet default private google access false",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output is missing %q:\n%s", want, out)
		}
	}
}

// TestShowReportsWhatIsMissing covers a fresh project: every missing piece is
// named instead of being provisioned behind the operator's back.
func TestShowReportsWhatIsMissing(t *testing.T) {
	p := newProbe(t, absent)
	if err := p.run(t, "show"); err != nil {
		t.Fatalf("network show: %v", err)
	}
	out := p.out.String()
	for _, want := range []string{
		"firewall devbox-ssh missing",
		"router devbox-router missing",
		"nat devbox-nat missing",
		"subnet default missing in us-central1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output is missing %q:\n%s", want, out)
		}
	}
}

// TestShowReportsPrivateGoogleAccessWhenOn covers the flag that decides whether a
// box needs the NAT to reach Google APIs.
func TestShowReportsPrivateGoogleAccessWhenOn(t *testing.T) {
	p := newProbe(t, func(args []string) (string, error) {
		if args[1] == "networks" {
			return `{"name":"default","privateIpGoogleAccess":true}`, nil
		}
		return "{}", nil
	})
	if err := p.run(t, "show"); err != nil {
		t.Fatalf("network show: %v", err)
	}
	if !strings.Contains(p.out.String(), "subnet default private google access true") {
		t.Fatalf("show output = %s", p.out.String())
	}
}

// TestUnknownVerbIsRefused covers the dispatcher: a verb this slice does not own
// is refused with the usage line rather than silently doing nothing.
func TestUnknownVerbIsRefused(t *testing.T) {
	p := newProbe(t, func([]string) (string, error) { return "", nil })
	err := p.run(t, "open")
	if err == nil || !strings.Contains(err.Error(), "devbox network <ensure|show>") {
		t.Fatalf("want a usage refusal, got %v", err)
	}
	if len(p.cloud.Calls()) != 0 {
		t.Fatalf("an unknown verb called the cloud:\n%s", p.cloud.Argv())
	}
}
