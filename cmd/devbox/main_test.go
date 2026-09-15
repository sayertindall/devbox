package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
	"devbox/internal/record"
)

// instance is a describe result for a box devbox created, with its data disk.
const instance = `[{
  "name": "dev",
  "zone": "https://www.googleapis.com/compute/v1/projects/example-project/zones/us-central1-a",
  "machineType": "https://www.googleapis.com/compute/v1/projects/example-project/zones/us-central1-a/machineTypes/n2-standard-16",
  "status": "RUNNING",
  "labels": {"devbox-managed": "true", "devbox-name": "dev"},
  "networkInterfaces": [{"networkIP": "10.0.0.2", "accessConfigs": []}],
  "disks": [
    {"deviceName": "persistent-disk-0", "boot": true, "source": "https://www.googleapis.com/compute/v1/projects/example-project/zones/us-central1-a/disks/dev"},
    {"deviceName": "devbox-data", "boot": false, "source": "https://www.googleapis.com/compute/v1/projects/example-project/zones/us-central1-a/disks/devbox-data"}
  ]
}]`

const disk = `[{
  "name": "devbox-data",
  "labels": {"devbox-managed": "true", "devbox-name": "dev"},
  "users": ["https://www.googleapis.com/compute/v1/projects/example-project/zones/us-central1-a/instances/dev"]
}]`

// errNotFound is what gcloud prints for an absent resource, and errDenied is a
// failure whose outcome devbox cannot interpret.
var (
	errNotFound = errors.New("ERROR: The resource 'projects/example-project/zones/us-central1-a/instances/dev' was not found")
	errDenied   = errors.New("ERROR: permission denied on resource")
)

// harness builds the dependencies a command line needs, with the state and the
// home directory redirected to a temporary directory so nothing real is touched.
func harness(t *testing.T, reply func([]string) (string, error), dryRun bool) (cli.Deps, *gcloud.Fake, *bytes.Buffer, *record.Store) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DEVBOX_HOME", home)
	t.Setenv("HOME", home)

	store, err := record.Open(home + "/records")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Project = "example-project"
	cfg.ServiceAccount = "devbox@example-project.iam.gserviceaccount.com"
	cfg.BootstrapURL = "gs://bucket/devbox/startup-script.sh"
	cfg.RemoteUser = "operator"

	cloud := &gcloud.Fake{Reply: reply}
	var out bytes.Buffer
	deps := cli.Deps{
		Config:     cfg,
		ConfigPath: home + "/config.toml",
		Cloud:      cloud,
		Records:    store,
		Out:        &out,
		Err:        &out,
		Stdin:      strings.NewReader(""),
		DryRun:     dryRun,
	}
	return deps, cloud, &out, store
}

func runDevbox(t *testing.T, deps cli.Deps, args ...string) error {
	t.Helper()
	return buildRegistry(deps).Run(context.Background(), deps, args)
}

func TestDryRunPrintsTheWholeCallAndRecordsNothing(t *testing.T) {
	reply := func(args []string) (string, error) {
		line := strings.Join(args, " ")
		switch {
		case strings.Contains(line, " describe "):
			return "", errNotFound
		case strings.Contains(line, "instances describe"):
			return "", errNotFound
		default:
			return "[]", nil
		}
	}
	deps, cloud, out, store := harness(t, reply, true)
	if err := runDevbox(t, deps, "network", "ensure"); err != nil {
		t.Fatalf("network ensure: %v", err)
	}
	if err := runDevbox(t, deps, "machine", "new", "dev"); err != nil {
		t.Fatalf("machine new: %v", err)
	}
	// The recorded cloud calls are the transcript a dry run would print, because
	// the printing lives in the real runner and this test drives a recording one.
	argv := cloud.Argv()
	// A rehearsal must not claim it created anything.
	text := out.String()
	if !strings.Contains(text, "would create box dev") {
		t.Fatalf("the rehearsal must say what it would do:\n%s", text)
	}
	if strings.Contains(text, "created box dev") {
		t.Fatalf("a rehearsal claimed it created the box:\n%s", text)
	}
	_ = argv
	for _, required := range []string{
		"compute firewall-rules create",
		"compute routers create",
		"compute routers nats create",
		"compute instances create dev",
		"--machine-type=n2-standard-16",
		"--image-family=debian-13",
		"--create-disk=name=devbox-data,size=1024GB,type=pd-ssd,device-name=devbox-data,auto-delete=no",
		"--no-address",
		"--metadata=enable-oslogin=TRUE,startup-script-url=gs://bucket/devbox/startup-script.sh",
		"--instance-termination-action=STOP",
	} {
		if !strings.Contains(argv, required) {
			t.Fatalf("the rehearsal is missing the call %q:\n%s", required, argv)
		}
	}
	entries, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a dry run must not record a mutation, otherwise it blocks the next real command: %+v", entries)
	}
}

func TestAmbiguousCreateBlocksTheBoxUntilItIsReconciled(t *testing.T) {
	created := false
	reply := func(args []string) (string, error) {
		line := strings.Join(args, " ")
		switch {
		case strings.Contains(line, "instances describe"):
			if created {
				return instance, nil
			}
			return "", errNotFound
		case strings.Contains(line, "instances create"):
			return "", errDenied
		default:
			return "[]", nil
		}
	}
	deps, _, out, store := harness(t, reply, false)

	if err := runDevbox(t, deps, "machine", "new", "dev"); err == nil {
		t.Fatal("a create that failed without a clear outcome must be reported")
	}
	if !strings.Contains(out.String(), "devbox reconcile") {
		t.Fatalf("the failure must name the command that clears it:\n%s", out.String())
	}
	unresolved, err := store.Unresolved("dev")
	if err != nil || len(unresolved) != 1 {
		t.Fatalf("an ambiguous create must leave exactly one blocking record: %+v %v", unresolved, err)
	}

	out.Reset()
	err = runDevbox(t, deps, "machine", "new", "dev")
	if err == nil {
		t.Fatal("a blocked box must refuse the next mutation")
	}
	if !strings.Contains(err.Error(), unresolved[0].ID) {
		t.Fatalf("the refusal must name the blocking record: %v", err)
	}

	out.Reset()
	if err := runDevbox(t, deps, "reconcile"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), unresolved[0].ID) {
		t.Fatalf("reconcile must list the blocking record:\n%s", out.String())
	}
	if err := runDevbox(t, deps, "reconcile", unresolved[0].ID, "--note", "instance exists"); err != nil {
		t.Fatal(err)
	}

	created = true
	if err := runDevbox(t, deps, "machine", "new", "dev"); err == nil {
		t.Fatal("the name is taken now, so new must refuse")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("new must report the taken name after reconciliation: %v", err)
	}
}

func TestDestroyShowsWhatIsAtStakeBeforeItRefuses(t *testing.T) {
	reply := boxWithDataDisk()
	deps, cloud, out, _ := harness(t, reply, true)

	unconfirmed := runDevbox(t, deps, "machine", "destroy", "dev")
	if unconfirmed == nil {
		t.Fatal("destroy without --confirm must refuse")
	}
	if !strings.Contains(out.String(), "would run: gcloud compute instances delete dev") {
		t.Fatalf("destroy must show what it would do before it checks the confirmation (error: %v):\n%s", unconfirmed, out.String())
	}
	if cloud.Ran("instances", "delete") {
		t.Fatal("a rehearsal must not delete anything")
	}
}

func TestDestroyDeletesTheBoxAndTheDiskItLabeled(t *testing.T) {
	deps, cloud, out, _ := harness(t, boxWithDataDisk(), false)

	if err := runDevbox(t, deps, "machine", "destroy", "dev", "--confirm=dev"); err != nil {
		t.Fatal(err)
	}
	if !cloud.Ran("instances", "delete", "dev") {
		t.Fatalf("destroy must delete the instance it owns:\n%s", cloud.Argv())
	}
	if !cloud.Ran("disks", "delete", "devbox-data") {
		t.Fatalf("destroy must delete the labeled data disk:\n%s", cloud.Argv())
	}
	if !strings.Contains(out.String(), "deleted instance dev") {
		t.Fatalf("destroy must report what it removed:\n%s", out.String())
	}
}

// boxWithDataDisk answers a describe with a labeled box that has one data disk,
// and a disk describe that proves the disk carries the box labels.
func boxWithDataDisk() func([]string) (string, error) {
	return func(args []string) (string, error) {
		line := strings.Join(args, " ")
		switch {
		case strings.Contains(line, "instances describe"):
			return instance, nil
		case strings.Contains(line, "disks describe"):
			return disk, nil
		case strings.Contains(line, "machine-images list"):
			return "[]", nil
		default:
			return "[]", nil
		}
	}
}

func TestEveryCommandIsRegisteredOnce(t *testing.T) {
	deps, _, _, _ := harness(t, func([]string) (string, error) { return "", nil }, true)
	registry := buildRegistry(deps)
	names := map[string]bool{}
	for _, command := range registry.Commands() {
		if names[command.Name] {
			t.Fatalf("command %q is registered twice", command.Name)
		}
		names[command.Name] = true
	}
	for _, expected := range []string{"machine", "network", "ssh", "exec", "push", "pull", "agent", "tools", "bootstrap", "reconcile", "config", "doctor", "ssh-config"} {
		if !names[expected] {
			t.Fatalf("command %q is missing from the dispatcher", expected)
		}
	}
}

// partialConfig writes the smallest configuration somebody gets from
// `devbox config init`, with the settings a machine needs still blank.
func partialConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DEVBOX_HOME", home)
	t.Setenv("DEVBOX_DRY_RUN", "1")
	path := filepath.Join(home, "config.toml")
	cfg := config.Default()
	cfg.Project = "example-project"
	if _, err := config.Init(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

// completeConfig writes a configuration with every setting a machine needs.
func completeConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DEVBOX_HOME", home)
	t.Setenv("DEVBOX_DRY_RUN", "1")
	path := filepath.Join(home, "config.toml")
	cfg := config.Default()
	cfg.Project = "example-project"
	cfg.ServiceAccount = "devbox@example-project.iam.gserviceaccount.com"
	cfg.BootstrapURL = "gs://bucket/devbox/startup-script.sh"
	cfg.RemoteUser = "operator"
	if _, err := config.Init(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIncompleteConfigurationAllowsOnlyTheCommandsThatSetItUp(t *testing.T) {
	path := partialConfig(t)

	err := runWith(context.Background(), []string{"machine", "list"})
	if err == nil {
		t.Fatal("a command that creates a machine must refuse an incomplete configuration")
	}
	for _, expected := range []string{path, "service_account", "blank"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("the refusal must name %q: %v", expected, err)
		}
	}

	for _, args := range [][]string{
		{"tools", "list"},
		{"help", "machine"},
		{"version"},
		{"config", "show"},
		{"reconcile"},
	} {
		line := args
		captureStdout(t, func() {
			if err := runWith(context.Background(), line); err != nil {
				t.Fatalf("devbox %s must work before a project is filled in: %v", strings.Join(line, " "), err)
			}
		})
	}
}

func TestHelpDescribesOneCommand(t *testing.T) {
	partialConfig(t)
	out := captureStdout(t, func() {
		if err := runWith(context.Background(), []string{"help", "machine"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, expected := range []string{"devbox machine <new|start|", "--confirm=<name>", "--no-bootstrap"} {
		if !strings.Contains(out, expected) {
			t.Fatalf("help does not describe the command (%q missing):\n%s", expected, out)
		}
	}

	out = captureStdout(t, func() {
		if err := runWith(context.Background(), []string{"machine", "--help"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "devbox machine <new|start|") {
		t.Fatalf("a command must answer --help itself:\n%s", out)
	}
}

func TestDryRunRehearsesInsteadOfConnecting(t *testing.T) {
	completeConfig(t)
	out := captureStdout(t, func() {
		if err := runWith(context.Background(), []string{"exec", "dev", "--", "git", "log", "--oneline"}); err != nil {
			t.Fatalf("a rehearsal must not need a box: %v", err)
		}
	})
	if !strings.Contains(out, "would run: ssh -o BatchMode=yes devbox-dev -- git log --oneline") {
		t.Fatalf("the rehearsal must print the command it would run:\n%s", out)
	}
}

// captureStdout collects what a command line prints, by swapping the process
// streams the dispatcher writes to.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = original }()
	fn()
	writer.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
