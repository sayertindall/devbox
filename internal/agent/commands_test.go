package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/packet"
	"devbox/internal/record"
)

// testBox is one command under test: the dependencies it reads, the recording
// session it reaches the box with, and the streams it writes to.
type testBox struct {
	deps    cli.Deps
	records *record.Store
	session *access.Recording
	out     *bytes.Buffer
	errOut  *bytes.Buffer
}

// newTestBox builds a command environment rooted in temporary directories, so a
// test never reads or writes the operator's own devbox state.
func newTestBox(t *testing.T, stdin string) *testBox {
	t.Helper()
	t.Setenv("DEVBOX_HOME", t.TempDir())
	records, err := record.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	return &testBox{
		deps: cli.Deps{
			Config:  config.Default(),
			Records: records,
			Out:     out,
			Err:     errOut,
			Stdin:   strings.NewReader(stdin),
		},
		records: records,
		session: &access.Recording{},
		out:     out,
		errOut:  errOut,
	}
}

// open hands a verb the recording session instead of a box.
func (h *testBox) open(context.Context, box.Name) (access.Session, error) { return h.session, nil }

// startSessionRecord writes the durable note a start leaves behind, so a test can
// put a box in a state the command under test has to react to.
func (h *testBox) startSessionRecord(t *testing.T, request request, ref string) record.Record {
	t.Helper()
	entry, err := h.records.Begin(record.KindAgent, string(request.Box), sessionArgs(request, ref, launchCommand(request, ref)))
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// request builds a validated start for a test.
func testRequest(t *testing.T, boxName, provider, task, tree string) request {
	t.Helper()
	request, err := newRequest(box.Name(boxName), provider, task, tree, "")
	if err != nil {
		t.Fatalf("newRequest() error = %v", err)
	}
	return request
}

func TestStartUploadsTheHandoffBeforeTheSessionStarts(t *testing.T) {
	h := newTestBox(t, "")
	task := `fix it's "$HOME" now`
	uploadsAtStart := -1
	h.session.Reply = func(command string) (string, error) {
		if strings.HasPrefix(command, "tmux new-session") {
			uploadsAtStart = len(h.session.Uploads)
		}
		return "", nil
	}

	if err := start(context.Background(), h.deps, []string{"bedrock", "--provider", "omp", "--task", task, "--tree", "work"}, h.open); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if uploadsAtStart != 1 {
		t.Fatalf("the session started after %d uploads, want the handoff uploaded first", uploadsAtStart)
	}

	entries, err := h.records.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("records = %d, want one note for the start", len(entries))
	}
	session, ok := parseSessionRecord(entries[0])
	if !ok {
		t.Fatalf("record %+v names no session", entries[0])
	}
	if entries[0].State != record.StateKnown || entries[0].Result != sessionName(session.Ref) {
		t.Fatalf("record state = %s result = %q, want known with %s", entries[0].State, entries[0].Result, sessionName(session.Ref))
	}
	if session.Provider != ProviderOmp || session.Tree != "work" {
		t.Fatalf("record names provider %q tree %q, want omp and work", session.Provider, session.Tree)
	}

	wantCommands := []string{
		`test -d "$HOME/devbox/trees/work"`,
		`mkdir -p "$HOME/devbox/agents/` + session.Ref + `"`,
	}
	if len(h.session.Commands) != 3 {
		t.Fatalf("commands = %q, want the directory check, the session directory, and the start", h.session.Commands)
	}
	for index, want := range wantCommands {
		if h.session.Commands[index] != want {
			t.Errorf("command %d = %q, want %q", index, h.session.Commands[index], want)
		}
	}
	launch := h.session.Commands[2]
	if !strings.HasPrefix(launch, "tmux new-session -d -s devbox-"+session.Ref+" '") {
		t.Errorf("launch = %q, want a detached tmux session named devbox-%s", launch, session.Ref)
	}
	if !strings.Contains(launch, `'\''fix it'\''\'\'''\''s "$HOME" now'\''`) {
		t.Errorf("launch = %q, want the task quoted for every shell", launch)
	}

	local, err := localHandoff("bedrock", session.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.session.Uploads) != 1 || h.session.Uploads[0] != [2]string{local, remoteSessionDir(session.Ref)} {
		t.Fatalf("uploads = %v, want %s into %s", h.session.Uploads, local, remoteSessionDir(session.Ref))
	}
	handoff := decodeHandoff(t, local)
	if err := handoff.Validate(); err != nil {
		t.Fatalf("the packet on disk is not usable: %v", err)
	}
	if handoff.Task != task || handoff.Box != "bedrock" {
		t.Fatalf("packet task = %q box = %q, want the task and bedrock", handoff.Task, handoff.Box)
	}
	if !strings.Contains(h.out.String(), "started "+sessionName(session.Ref)) {
		t.Errorf("output = %q, want the start reported", h.out.String())
	}
}

func TestStartRefusesWhileAnEarlierSessionIsUnresolved(t *testing.T) {
	h := newTestBox(t, "")
	request := testRequest(t, "bedrock", "omp", "work the task", "work")
	entry := h.startSessionRecord(t, request, "aaaabbbbccccdddd")

	err := start(context.Background(), h.deps, []string{"bedrock", "--provider", "omp", "--task", "another task", "--tree", "work"}, h.open)
	if err == nil {
		t.Fatal("start() succeeded while an earlier session was unresolved")
	}
	if !strings.Contains(err.Error(), "aaaabbbbccccdddd") {
		t.Errorf("refusal = %q, want the blocking session id", err)
	}
	want := "devbox reconcile " + entry.ID + ` --note "what you checked"`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal = %q, want %q", err, want)
	}
	if len(h.session.Commands) != 0 || len(h.session.Uploads) != 0 {
		t.Fatalf("the refusal still reached the box: %q %v", h.session.Commands, h.session.Uploads)
	}
	entries, err := h.records.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("records = %d, want the refused start to add none", len(entries))
	}

	// The refusal is per provider: another harness has no session on this box.
	if err := start(context.Background(), h.deps, []string{"bedrock", "--provider", "claude", "--task", "another task", "--tree", "work"}, h.open); err != nil {
		t.Fatalf("start() for another provider error = %v", err)
	}
}

func TestStartRefusesWhenTheWorkingDirectoryIsMissing(t *testing.T) {
	h := newTestBox(t, "")
	h.session.Fail = "test -d"

	err := start(context.Background(), h.deps, []string{"bedrock", "--provider", "omp", "--task", "work the task", "--tree", "work"}, h.open)
	if err == nil {
		t.Fatal("start() succeeded without the tree on the box")
	}
	if !strings.Contains(err.Error(), "devbox push bedrock --tree work") {
		t.Errorf("refusal = %q, want the exact push command", err)
	}
	entries, err := h.records.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("records = %d, want a refused start to leave no note", len(entries))
	}
	if len(h.session.Commands) != 1 || len(h.session.Uploads) != 0 {
		t.Fatalf("a refused start did more than check the directory: %q %v", h.session.Commands, h.session.Uploads)
	}
}

func TestStartLeavesAnUnresolvedRecordWhenTheOutcomeIsLost(t *testing.T) {
	h := newTestBox(t, "")
	h.session.Fail = "tmux new-session"

	err := start(context.Background(), h.deps, []string{"bedrock", "--provider", "omp", "--task", "work the task", "--tree", "work"}, h.open)
	if err == nil {
		t.Fatal("start() succeeded although the session command failed")
	}
	entries, err := h.records.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].State != record.StateUnknown {
		t.Fatalf("records = %+v, want one unresolved note: tmux may have created the session", entries)
	}
	if entries[0].Detail == "" {
		t.Fatal("the unresolved note carries no detail about what failed")
	}
	request := testRequest(t, "bedrock", "omp", "work the task", "work")
	if err := refusal(h.records, request); err == nil {
		t.Fatal("a second start was allowed after an unresolved session")
	}
}

func TestListMergesRecordsWithTmux(t *testing.T) {
	h := newTestBox(t, "")
	request := testRequest(t, "bedrock", "omp", "work the task", "work")
	running := h.startSessionRecord(t, request, "aaaabbbbccccdddd")
	if err := h.records.Known(running, sessionName("aaaabbbbccccdddd")); err != nil {
		t.Fatal(err)
	}
	stopped := h.startSessionRecord(t, request, "eeeeffff00001111")
	if err := h.records.Known(stopped, sessionName("eeeeffff00001111")); err != nil {
		t.Fatal(err)
	}
	h.startSessionRecord(t, request, "2222333344445555")
	lost := h.startSessionRecord(t, request, "aaaabbbb11112222")
	if err := h.records.Unknown(lost, "the connection dropped after the start"); err != nil {
		t.Fatal(err)
	}
	neverStarted := h.startSessionRecord(t, request, "5555666677778888")
	if err := h.records.Failed(neverStarted, "the session directory could not be created"); err != nil {
		t.Fatal(err)
	}

	h.session.Reply = func(string) (string, error) {
		return "devbox-aaaabbbbccccdddd 1757930123\ndevbox-9999888877776666 1757930000\nothertool 1757930000\n", nil
	}
	if err := list(context.Background(), h.deps, []string{"bedrock"}, h.open); err != nil {
		t.Fatalf("list() error = %v", err)
	}
	output := h.out.String()
	for _, want := range []struct{ id, provider, tree, state string }{
		{"aaaabbbbccccdddd", "omp", "work", "running"},
		{"eeeeffff00001111", "omp", "work", "stopped"},
		{"2222333344445555", "omp", "work", "unresolved"},
		{"aaaabbbb11112222", "omp", "work", "unresolved"},
		{"5555666677778888", "omp", "work", "failed"},
		{"9999888877776666", "-", "-", "unrecorded"},
	} {
		assertRow(t, output, want.id, want.provider, want.tree, want.state)
	}
	// An unresolved session blocks a start until it is cleared, so the listing
	// names the record that holds it and the command that clears it.
	if !strings.Contains(output, "session 2222333344445555 is unresolved and blocks the next omp start") {
		t.Errorf("list output does not say that the pending session blocks a start:\n%s", output)
	}
	if !strings.Contains(output, "devbox reconcile "+lost.ID+` --note "what you checked"`) {
		t.Errorf("list output does not name the reconcile command for %s:\n%s", lost.ID, output)
	}
	if strings.Contains(output, "othertool") {
		t.Errorf("list output reports a session devbox did not start:\n%s", output)
	}
}

func TestStopRecordsTheSessionOnlyAfterTheKillSucceeds(t *testing.T) {
	h := newTestBox(t, "")
	request := testRequest(t, "bedrock", "omp", "work the task", "work")

	failing := h.startSessionRecord(t, request, "aaaabbbbccccdddd")
	if err := h.records.Known(failing, sessionName("aaaabbbbccccdddd")); err != nil {
		t.Fatal(err)
	}
	h.session.Fail = "tmux kill-session"
	if err := stop(context.Background(), h.deps, []string{"bedrock", "aaaabbbbccccdddd", "--yes"}, h.open); err == nil {
		t.Fatal("stop() succeeded although the kill failed")
	}
	after, err := h.records.Load(failing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State == record.StateKnown {
		t.Fatalf("the record reached known although the kill was not confirmed: %+v", after)
	}

	h.session.Fail = ""
	stopping := h.startSessionRecord(t, request, "eeeeffff00001111")
	if err := h.records.Known(stopping, sessionName("eeeeffff00001111")); err != nil {
		t.Fatal(err)
	}
	h.session.Reply = func(command string) (string, error) {
		if strings.HasPrefix(command, "if [ -f") {
			return "done\n", nil
		}
		return "", nil
	}
	if err := stop(context.Background(), h.deps, []string{"bedrock", "eeeeffff00001111", "--yes"}, h.open); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	after, err = h.records.Load(stopping.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != record.StateKnown || after.Result != sessionName("eeeeffff00001111") {
		t.Fatalf("record = %+v, want known with the session name", after)
	}
	if !strings.Contains(h.out.String(), "status done") {
		t.Errorf("output = %q, want the status file the session wrote reported", h.out.String())
	}
}

func TestStopAsksBeforeItKills(t *testing.T) {
	h := newTestBox(t, "no\n")
	request := testRequest(t, "bedrock", "omp", "work the task", "work")
	entry := h.startSessionRecord(t, request, "aaaabbbbccccdddd")
	if err := h.records.Known(entry, sessionName("aaaabbbbccccdddd")); err != nil {
		t.Fatal(err)
	}

	err := stop(context.Background(), h.deps, []string{"bedrock", "aaaabbbbccccdddd"}, h.open)
	if err == nil {
		t.Fatal("stop() killed a session the operator did not confirm")
	}
	if len(h.session.Commands) != 0 {
		t.Fatalf("the box heard %q before the operator confirmed", h.session.Commands)
	}
	if !strings.Contains(h.out.String(), "devbox-aaaabbbbccccdddd") || !strings.Contains(h.out.String(), "provider omp, tree work") {
		t.Errorf("stop output does not say what it affects: %q", h.out.String())
	}
	after, err := h.records.Load(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != record.StateKnown {
		t.Fatalf("record = %+v, want the cancelled kill to leave it alone", after)
	}
}

func TestStopRefusesASessionDevboxDidNotStart(t *testing.T) {
	h := newTestBox(t, "")
	err := stop(context.Background(), h.deps, []string{"bedrock", "aaaabbbbccccdddd", "--yes"}, h.open)
	if err == nil {
		t.Fatal("stop() killed a session devbox has no record of")
	}
	if len(h.session.Commands) != 0 {
		t.Fatalf("the box heard %q for an unrecorded session", h.session.Commands)
	}
}

func TestLogsReadsThePane(t *testing.T) {
	h := newTestBox(t, "")
	h.session.Reply = func(string) (string, error) { return "first line\nsecond line\n", nil }

	if err := logs(context.Background(), h.deps, []string{"bedrock", "aaaabbbbccccdddd"}, h.open); err != nil {
		t.Fatalf("logs() error = %v", err)
	}
	if len(h.session.Commands) != 1 || h.session.Commands[0] != "tmux capture-pane -p -t devbox-aaaabbbbccccdddd -S -200" {
		t.Fatalf("commands = %q, want one pane capture", h.session.Commands)
	}
	if got := h.out.String(); got != "first line\nsecond line\n" {
		t.Fatalf("logs output = %q, want the pane", got)
	}
}

func TestAttachRunsInteractiveSSH(t *testing.T) {
	h := newTestBox(t, "")
	var got []string
	run := func(_ context.Context, argv []string, _ io.Reader, _, _ io.Writer) error {
		got = argv
		return nil
	}
	if err := attach(context.Background(), h.deps, []string{"bedrock", "aaaabbbbccccdddd"}, h.open, run); err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	want := []string{"ssh", "-t", "devbox-bedrock", "tmux attach -t devbox-aaaabbbbccccdddd"}
	if len(got) != len(want) {
		t.Fatalf("attach ran %q, want %q", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("attach ran %q, want %q", got, want)
		}
	}
}

// assertRow checks one session line by field, so an assertion does not depend on
// how wide the columns happen to be.
func assertRow(t *testing.T, output, id, provider, tree, state string) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != id {
			continue
		}
		if fields[1] != provider || fields[2] != tree || fields[3] != state {
			t.Fatalf("row for %s = %q, want provider %s tree %s state %s", id, line, provider, tree, state)
		}
		if fields[4] == "" {
			t.Fatalf("row for %s reports no age", id)
		}
		return
	}
	t.Fatalf("no row for %s in:\n%s", id, output)
}

// decodeHandoff reads the packet devbox wrote for the operator.
func decodeHandoff(t *testing.T, path string) packet.Handoff {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var handoff packet.Handoff
	if err := json.Unmarshal(data, &handoff); err != nil {
		t.Fatalf("decode %s: %v", filepath.Base(path), err)
	}
	return handoff
}
