package bootstrap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
	"devbox/internal/record"
)

// rig is one command under test with a recorded cloud, a real record store in a
// temporary directory, and the state directory redirected away from the
// operator's own.
type rig struct {
	deps    cli.Deps
	cloud   *gcloud.Fake
	records *record.Store
	out     *bytes.Buffer
	errOut  *bytes.Buffer
}

func newRig(t *testing.T, cfg config.Config) *rig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DEVBOX_HOME", home)
	records, err := record.Open(filepath.Join(home, "records"))
	if err != nil {
		t.Fatalf("open record store: %v", err)
	}
	r := &rig{
		cloud:   &gcloud.Fake{},
		records: records,
		out:     &bytes.Buffer{},
		errOut:  &bytes.Buffer{},
	}
	r.deps = cli.Deps{
		Config:     cfg,
		ConfigPath: filepath.Join(home, "config.toml"),
		Cloud:      r.cloud,
		Records:    records,
		Out:        r.out,
		Err:        r.errOut,
		Stdin:      strings.NewReader(""),
	}
	return r
}

// operatingConfig is a configuration that passes validation, so a test can save
// it and observe what a command writes back.
func operatingConfig() config.Config {
	cfg := config.Default()
	cfg.Project = "example-project"
	cfg.ServiceAccount = "devbox@example-project.iam.gserviceaccount.com"
	cfg.RemoteUser = "operator"
	return cfg
}

func command(t *testing.T, name string) cli.Command {
	t.Helper()
	for _, candidate := range Commands() {
		if candidate.Name == name {
			return candidate
		}
	}
	t.Fatalf("no %s command is registered", name)
	return cli.Command{}
}

func TestBootstrapShowPrintsTheRenderedScript(t *testing.T) {
	cfg := operatingConfig()
	r := newRig(t, cfg)
	if err := command(t, "bootstrap").Run(context.Background(), r.deps, []string{"show"}); err != nil {
		t.Fatalf("bootstrap show: %v", err)
	}
	want, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if r.out.String() != want {
		t.Error("bootstrap show printed something other than the rendered script")
	}
	if len(r.cloud.Calls()) != 0 {
		t.Errorf("bootstrap show reached the cloud: %s", r.cloud.Argv())
	}
}

func TestBootstrapShowRejectsArguments(t *testing.T) {
	r := newRig(t, operatingConfig())
	err := command(t, "bootstrap").Run(context.Background(), r.deps, []string{"show", "extra"})
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("bootstrap show accepted an argument: %v", err)
	}
	err = command(t, "bootstrap").Run(context.Background(), r.deps, nil)
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("bootstrap with no subverb: %v", err)
	}
	err = command(t, "bootstrap").Run(context.Background(), r.deps, []string{"publish"})
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("bootstrap accepted an unknown subverb: %v", err)
	}
}

// A local file is not a bootstrap url: the machine slice hands the value to
// gcloud as startup-script-url, which only takes a gs:// or https location. With
// no bucket the command therefore writes the script and prints the copy command
// instead of leaving a url that would fail on the next `devbox machine new`.
func TestBootstrapUploadWithoutABucketPublishesNothing(t *testing.T) {
	cfg := operatingConfig()
	r := newRig(t, cfg)
	if err := config.Save(r.deps.ConfigPath, cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}
	before, err := os.ReadFile(r.deps.ConfigPath)
	if err != nil {
		t.Fatalf("read configuration: %v", err)
	}

	if err := command(t, "bootstrap").Run(context.Background(), r.deps, []string{"upload"}); err != nil {
		t.Fatalf("bootstrap upload: %v", err)
	}

	script, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	path, err := config.StatePath(startupFileName)
	if err != nil {
		t.Fatalf("StatePath: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the published script: %v", err)
	}
	if string(written) != script {
		t.Error("the published script is not the rendered script")
	}
	if !strings.Contains(r.out.String(), "gcloud storage cp "+path+" gs://") {
		t.Errorf("the copy command is not printed:\n%s", r.out.String())
	}
	if len(r.cloud.Calls()) != 0 {
		t.Errorf("upload reached the cloud without a bucket: %s", r.cloud.Argv())
	}
	after, err := os.ReadFile(r.deps.ConfigPath)
	if err != nil {
		t.Fatalf("read configuration: %v", err)
	}
	if string(before) != string(after) {
		t.Error("upload rewrote a configuration it did not publish for")
	}
}

func TestBootstrapUploadPublishesAndRecordsTheURL(t *testing.T) {
	cfg := operatingConfig()
	r := newRig(t, cfg)
	if err := config.Save(r.deps.ConfigPath, cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}
	if err := command(t, "bootstrap").Run(context.Background(), r.deps, []string{"upload", "--bucket", "gs://example-devbox"}); err != nil {
		t.Fatalf("bootstrap upload: %v", err)
	}
	path, err := config.StatePath(startupFileName)
	if err != nil {
		t.Fatalf("StatePath: %v", err)
	}
	want := []string{"storage", "cp", path, "gs://example-devbox/" + startupFileName}
	got := r.cloud.Last()
	if len(got) != len(want) {
		t.Fatalf("the publish command is %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("the publish command is %v, want %v", got, want)
		}
	}
	saved, err := config.Load(r.deps.ConfigPath)
	if err != nil {
		t.Fatalf("reload configuration: %v", err)
	}
	if saved.BootstrapURL != "gs://example-devbox/"+startupFileName {
		t.Errorf("bootstrap_url is %q, want the published object", saved.BootstrapURL)
	}
}

// A second upload goes where the first one went, so iterating on the script does
// not need the bucket repeated.
func TestBootstrapUploadReusesTheConfiguredBucket(t *testing.T) {
	cfg := operatingConfig()
	cfg.BootstrapURL = "gs://existing-devbox/startup-script.sh"
	r := newRig(t, cfg)
	if err := config.Save(r.deps.ConfigPath, cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}
	if err := command(t, "bootstrap").Run(context.Background(), r.deps, []string{"upload"}); err != nil {
		t.Fatalf("bootstrap upload: %v", err)
	}
	if !r.cloud.Ran("storage", "cp", "gs://existing-devbox/"+startupFileName) {
		t.Errorf("upload did not publish to the configured bucket: %s", r.cloud.Argv())
	}
}

func TestBootstrapUploadDryRunRecordsNothing(t *testing.T) {
	cfg := operatingConfig()
	cfg.BootstrapURL = "gs://existing-devbox/startup-script.sh"
	r := newRig(t, cfg)
	r.deps.DryRun = true
	if err := config.Save(r.deps.ConfigPath, cfg); err != nil {
		t.Fatalf("save configuration: %v", err)
	}
	if err := command(t, "bootstrap").Run(context.Background(), r.deps, []string{"upload"}); err != nil {
		t.Fatalf("bootstrap upload: %v", err)
	}
	if len(r.cloud.Calls()) != 0 {
		t.Errorf("a dry run reached the cloud: %s", r.cloud.Argv())
	}
	if !strings.Contains(r.out.String(), "dry run: gcloud storage cp") {
		t.Errorf("a dry run did not print the command:\n%s", r.out.String())
	}
	saved, err := config.Load(r.deps.ConfigPath)
	if err != nil {
		t.Fatalf("reload configuration: %v", err)
	}
	if saved.BootstrapURL != cfg.BootstrapURL {
		t.Errorf("a dry run changed bootstrap_url to %q", saved.BootstrapURL)
	}
}

func TestBootstrapUploadRefusesABucketThatIsNotAStorageLocation(t *testing.T) {
	r := newRig(t, operatingConfig())
	err := command(t, "bootstrap").Run(context.Background(), r.deps, []string{"upload", "--bucket", "example-devbox"})
	if err == nil || !strings.Contains(err.Error(), "gs://") {
		t.Fatalf("upload accepted a bucket an instance cannot fetch from: %v", err)
	}
	if len(r.cloud.Calls()) != 0 {
		t.Errorf("upload reached the cloud for a rejected bucket: %s", r.cloud.Argv())
	}
	path, err := config.StatePath(startupFileName)
	if err != nil {
		t.Fatalf("StatePath: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("upload wrote %s before validating the bucket", path)
	}
}

// describeJSON is what gcloud returns for the describe call bake makes.
func describeJSON(status string, dataDisk bool) string {
	disks := `{"deviceName":"work","boot":true}`
	if dataDisk {
		disks = `{"deviceName":"devbox-data","boot":false},` + disks
	}
	return `[{"name":"work","zone":"projects/example-project/zones/us-central1-a",` +
		`"machineType":"projects/example-project/zones/us-central1-a/machineTypes/n2-standard-16",` +
		`"status":"` + status + `",` +
		`"labels":{"devbox-name":"work","devbox-managed":"true"},` +
		`"disks":[` + disks + `]}]`
}

func imageCommand(t *testing.T) cli.Command {
	t.Helper()
	return command(t, "image")
}

func TestImageBakeCreatesAnImageFromTheDataDisk(t *testing.T) {
	cfg := operatingConfig()
	r := newRig(t, cfg)
	r.cloud.Reply = func(args []string) (string, error) {
		if len(args) > 1 && args[1] == "instances" {
			return describeJSON("TERMINATED", true), nil
		}
		return "", nil
	}
	session := &access.Recording{}
	restore := openSession
	openSession = func(context.Context, cli.Deps, box.Name) (access.Session, error) {
		t.Error("bake opened a session on a stopped box")
		return session, nil
	}
	t.Cleanup(func() { openSession = restore })

	if err := imageCommand(t).Run(context.Background(), r.deps, []string{"bake", "work", "work-image"}); err != nil {
		t.Fatalf("image bake: %v", err)
	}
	want := []string{
		"compute", "images", "create", "work-image",
		"--project=example-project",
		"--source-disk=devbox-data",
		"--source-disk-zone=us-central1-a",
		"--labels=devbox-managed=true,devbox-name=work",
	}
	got := r.cloud.Last()
	if len(got) != len(want) {
		t.Fatalf("the image command is %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("the image command is %v, want %v", got, want)
		}
	}
	if len(session.Commands) != 0 {
		t.Errorf("bake ran commands on a stopped box: %v", session.Commands)
	}
	entries, err := r.records.All()
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("bake wrote %d records, want 1", len(entries))
	}
	if entries[0].Kind != record.KindSnapshot || entries[0].State != record.StateKnown || entries[0].Box != "work" {
		t.Errorf("the record is %+v, want a known snapshot record for work", entries[0])
	}
}

// A running box is quiesced around the capture, because a disk image taken under
// a writing engine can carry a torn layer.
func TestImageBakeQuiescesARunningBox(t *testing.T) {
	cfg := operatingConfig()
	r := newRig(t, cfg)
	r.cloud.Reply = func(args []string) (string, error) {
		if len(args) > 1 && args[1] == "instances" {
			return describeJSON("RUNNING", true), nil
		}
		return "", nil
	}
	session := &access.Recording{}
	restore := openSession
	openSession = func(_ context.Context, deps cli.Deps, name box.Name) (access.Session, error) {
		if name != "work" {
			t.Errorf("bake opened a session on %s", name)
		}
		if deps.Cloud != r.cloud {
			t.Error("bake opened a session without the recorded cloud, so the dialer cannot describe the box")
		}
		return session, nil
	}
	t.Cleanup(func() { openSession = restore })

	if err := imageCommand(t).Run(context.Background(), r.deps, []string{"bake", "work", "work-image"}); err != nil {
		t.Fatalf("image bake: %v", err)
	}
	if len(session.Commands) != 3 {
		t.Fatalf("bake ran %v, want a stop, a flush, and a start", session.Commands)
	}
	if session.Commands[0] != quiesceStop || session.Commands[1] != quiesceFlush || session.Commands[2] != quiesceStart {
		t.Errorf("bake bracketed the capture with %v", session.Commands)
	}
	if !r.cloud.Ran("images", "create", "work-image") {
		t.Error("bake did not create the image")
	}
}

func TestImageBakeRefusesWhatItCannotCapture(t *testing.T) {
	cases := []struct {
		what    string
		reply   string
		name    string
		image   string
		blocked bool
		wantErr string
	}{
		{
			what:    "a machine devbox did not label",
			reply:   `[{"name":"work","status":"TERMINATED","labels":{"someone":"else"},"disks":[{"deviceName":"devbox-data","boot":false}]}]`,
			name:    "work",
			image:   "work-image",
			wantErr: "not labeled",
		},
		{
			what:    "a box with no data disk",
			reply:   describeJSON("TERMINATED", false),
			name:    "work",
			image:   "work-image",
			wantErr: "no attached data disk",
		},
		{
			what:    "a blocked box",
			reply:   describeJSON("TERMINATED", true),
			name:    "work",
			image:   "work-image",
			blocked: true,
			wantErr: "devbox reconcile",
		},
	}
	for _, item := range cases {
		t.Run(item.what, func(t *testing.T) {
			r := newRig(t, operatingConfig())
			r.cloud.Reply = func([]string) (string, error) { return item.reply, nil }
			if item.blocked {
				if _, err := r.records.Begin(record.KindCreate, "work", []string{"compute", "instances", "create", "work"}); err != nil {
					t.Fatalf("begin record: %v", err)
				}
			}
			err := imageCommand(t).Run(context.Background(), r.deps, []string{"bake", item.name, item.image})
			if err == nil {
				t.Fatalf("bake accepted %s", item.what)
			}
			if !strings.Contains(err.Error(), item.wantErr) {
				t.Errorf("error %q does not mention %q", err, item.wantErr)
			}
			if r.cloud.Ran("images", "create") {
				t.Errorf("bake created an image for %s", item.what)
			}
		})
	}
}

func TestImageBakeRefusesNamesAndUsage(t *testing.T) {
	cases := []struct {
		what  string
		args  []string
		want  string
		calls bool
	}{
		{what: "no arguments", args: nil, want: "usage"},
		{what: "one argument", args: []string{"bake", "work"}, want: "usage"},
		{what: "an unknown subverb", args: []string{"freeze", "work", "work-image"}, want: "usage"},
		{what: "an invalid box name", args: []string{"bake", "Work_1", "work-image"}, want: "box name"},
		{what: "an invalid image name", args: []string{"bake", "work", "Work_1"}, want: "image name"},
		{what: "a trailing hyphen", args: []string{"bake", "work", "work-image-"}, want: "hyphen"},
	}
	for _, item := range cases {
		t.Run(item.what, func(t *testing.T) {
			r := newRig(t, operatingConfig())
			err := imageCommand(t).Run(context.Background(), r.deps, item.args)
			if err == nil {
				t.Fatalf("bake accepted %s", item.what)
			}
			if !strings.Contains(err.Error(), item.want) {
				t.Errorf("error %q does not mention %q", err, item.want)
			}
			if len(r.cloud.Calls()) != 0 {
				t.Errorf("bake reached the cloud for %s: %s", item.what, r.cloud.Argv())
			}
		})
	}
}

func TestToolchainPrintsWhatABoxInstalls(t *testing.T) {
	cfg := operatingConfig()
	r := newRig(t, cfg)
	if err := command(t, "toolchain").Run(context.Background(), r.deps, nil); err != nil {
		t.Fatalf("toolchain: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(r.out.String()), "\n")
	if len(lines) != len(DefaultTools())+1 {
		t.Fatalf("toolchain printed %d lines, want %d", len(lines), len(DefaultTools())+1)
	}
	for index, name := range config.SortedTools(DefaultTools()) {
		want := name + " " + DefaultTools()[name]
		if lines[index] != want {
			t.Errorf("line %d is %q, want %q", index, lines[index], want)
		}
	}
	if last := lines[len(lines)-1]; last != "harness "+config.DefaultOmpPackage+" "+config.DefaultOmpVersion {
		t.Errorf("the harness line is %q", last)
	}
}

func TestToolchainPrintsTheConfiguredPins(t *testing.T) {
	cfg := operatingConfig()
	cfg.Tools = map[string]string{"go": "1.25.0"}
	r := newRig(t, cfg)
	if err := command(t, "toolchain").Run(context.Background(), r.deps, nil); err != nil {
		t.Fatalf("toolchain: %v", err)
	}
	if !strings.Contains(r.out.String(), "go 1.25.0") {
		t.Errorf("toolchain did not print the configured pin:\n%s", r.out.String())
	}
	if strings.Contains(r.out.String(), "node") {
		t.Errorf("toolchain printed a tool the configuration does not declare:\n%s", r.out.String())
	}
	err := command(t, "toolchain").Run(context.Background(), r.deps, []string{"list"})
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("toolchain accepted an argument: %v", err)
	}
}
