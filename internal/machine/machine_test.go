package machine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
	"devbox/internal/record"
)

// probe is one test's dependencies: a configuration, a recorded cloud, and a
// record store in a temporary directory, so no test can reach a project or leave
// state behind.
type probe struct {
	deps    cli.Deps
	cloud   *gcloud.Fake
	records *record.Store
	out     *bytes.Buffer
	errOut  *bytes.Buffer
}

func newProbe(t *testing.T) *probe {
	t.Helper()
	cfg := config.Default()
	cfg.Project = "devbox-probe"
	cfg.ServiceAccount = "devbox@devbox-probe.iam.gserviceaccount.com"
	cfg.BootstrapURL = "gs://devbox-bootstrap/startup.sh"
	store, err := record.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open record store: %v", err)
	}
	cloud := &gcloud.Fake{}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	return &probe{
		deps: cli.Deps{
			Config:     cfg,
			ConfigPath: "/tmp/devbox/config.toml",
			Cloud:      cloud,
			Records:    store,
			Out:        out,
			Err:        errOut,
			Stdin:      strings.NewReader(""),
		},
		cloud:   cloud,
		records: store,
		out:     out,
		errOut:  errOut,
	}
}

// run dispatches one verb through the registered command, so a test exercises the
// same path the binary does.
func (p *probe) run(t *testing.T, args ...string) error {
	t.Helper()
	command, ok := findCommand("machine")
	if !ok {
		t.Fatal("the machine command is not registered")
	}
	return command.Run(context.Background(), p.deps, args)
}

// describeFor answers the instance describes for the named boxes and reports
// every other instance as absent until a create call names it, which is what new
// and fork read to decide that a name is free and again to read the box back.
func (p *probe) describeFor(status string, names ...string) {
	known := map[string]bool{}
	for _, name := range names {
		known[name] = true
	}
	created := map[string]bool{}
	p.cloud.Reply = func(args []string) (string, error) {
		switch {
		case isVerb(args, "create"):
			created[args[3]] = true
			return "", nil
		case isVerb(args, "describe"):
			if known[args[3]] || created[args[3]] {
				return instanceJSON(args[3], status), nil
			}
			return "", missing("instance")
		}
		return "", nil
	}
}

// flagValue returns the value of one flag in a recorded argument vector, or the
// empty string when the call did not carry it.
func flagValue(argv []string, name string) string {
	for _, arg := range argv {
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"=")
		}
	}
	return ""
}

func findCommand(name string) (cli.Command, bool) {
	for _, command := range Commands() {
		if command.Name == name {
			return command, true
		}
	}
	return cli.Command{}, false
}

// withSession runs the verbs that need a shell session against a recording
// session, so a snapshot test never needs a live box.
func withSession(t *testing.T, session access.Session) {
	t.Helper()
	previous := openSession
	openSession = func(cli.Deps, box.Name) (access.Session, error) { return session, nil }
	t.Cleanup(func() { openSession = previous })
}

// instanceJSON is one instance as gcloud describes it, with the labels devbox
// writes, an internal address, and a boot disk plus a data disk.
func instanceJSON(name, status string) string {
	return fmt.Sprintf(`[{"name":%q,"zone":"https://www.googleapis.com/compute/v1/projects/devbox-probe/zones/us-central1-a","machineType":"https://www.googleapis.com/compute/v1/projects/devbox-probe/zones/us-central1-a/machineTypes/n2-standard-16","status":%q,"creationTimestamp":"2026-09-15T12:00:00.000-07:00","labels":{"devbox-managed":"true","devbox-name":%q},"networkInterfaces":[{"networkIP":"10.128.0.2"}],"disks":[{"deviceName":"persistent-disk-0","source":"https://www.googleapis.com/compute/v1/projects/devbox-probe/zones/us-central1-a/disks/%s","boot":true},{"deviceName":"devbox-data","source":"https://www.googleapis.com/compute/v1/projects/devbox-probe/zones/us-central1-a/disks/devbox-data","boot":false}]}]`,
		name, status, name, name)
}

// isVerb reports whether a recorded call is a Compute instances call with the
// given verb.
func isVerb(args []string, verb string) bool {
	return len(args) > 2 && args[0] == "compute" && args[1] == "instances" && args[2] == verb
}

// call finds the first recorded call inside a Compute group with the given verb,
// so a test can pick a mutation out of the describes around it.
func call(t *testing.T, cloud *gcloud.Fake, group, verb string) []string {
	t.Helper()
	for _, recorded := range cloud.Calls() {
		if len(recorded) > 2 && recorded[1] == group && recorded[2] == verb {
			return recorded
		}
	}
	t.Fatalf("no %s %s call was made; calls:\n%s", group, verb, cloud.Argv())
	return nil
}

// position is where a call carrying every fragment happened in the recorded
// sequence, so a test can prove one call came before another.
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

// hasCall reports whether any recorded call is a Compute group call with the
// given verb, which is how a test proves that nothing was deleted.
func hasCall(cloud *gcloud.Fake, group, verb string) bool {
	for _, recorded := range cloud.Calls() {
		if len(recorded) > 2 && recorded[1] == group && recorded[2] == verb {
			return true
		}
	}
	return false
}

// missing is the error shape gcloud reports an absent resource in, which is what
// gcloud.Missing recognizes.
func missing(what string) error { return fmt.Errorf("%s was not found", what) }
