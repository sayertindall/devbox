package agent

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"devbox/internal/access"
	"devbox/internal/box"
	"devbox/internal/cli"
)

// tmuxSession is one live session as tmux reports it.
type tmuxSession struct {
	Ref       string
	CreatedAt time.Time
}

// liveSessions asks the box what is running.
//
// A box that has never started a session has no tmux server, which tmux reports
// as a failed command. That is not an error for a listing, so the failure is
// reported on the error stream and the caller carries on with the local records:
// the records are the part of the answer devbox knows for certain.
func liveSessions(ctx context.Context, deps cli.Deps, session access.Session, boxName box.Name) []tmuxSession {
	output, err := session.Run(ctx, listCommand())
	if err != nil {
		deps.Errorf("tmux ls on %s: %v", boxName, err)
		return nil
	}
	return parseSessions(output)
}

// parseSessions reads the name and creation time tmux prints for each session.
//
// tmux lists sessions devbox did not start as well, so a name without the devbox
// prefix is skipped: a listing must never suggest devbox could stop it.
func parseSessions(output string) []tmuxSession {
	var out []tmuxSession
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], sessionPrefix) {
			continue
		}
		ref := strings.TrimPrefix(fields[0], sessionPrefix)
		if _, err := ParseSessionRef(ref); err != nil {
			continue
		}
		created, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			created = 0
		}
		session := tmuxSession{Ref: ref}
		if created > 0 {
			session.CreatedAt = time.Unix(created, 0).UTC()
		}
		out = append(out, session)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// sessionState is the one word that tells the operator what a row means: tmux and
// the record store answer different questions, and the operator needs both.
//
// An unresolved note wins, because that is the only state the operator has to act
// on. After that tmux is the truth about liveness: a session tmux reports is
// running whatever the note says, and only a session that is not running is
// reported from the record alone.
func sessionState(session sessionRecord, live bool) string {
	switch {
	case session.unresolved():
		return "unresolved"
	case live:
		return "running"
	case session.Entry.Result == "":
		return "failed"
	default:
		return "stopped"
	}
}

// render prints one row per session, merging the durable notes with what tmux
// reports. A session tmux reports but devbox never recorded is shown too, so the
// operator can see it, but devbox will not stop a session it did not start.
func render(out io.Writer, boxName box.Name, records []sessionRecord, live []tmuxSession) error {
	now := time.Now().UTC()
	byRef := make(map[string]sessionRecord, len(records))
	for _, session := range records {
		byRef[session.Ref] = session
	}
	liveRefs := make(map[string]tmuxSession, len(live))
	for _, session := range live {
		liveRefs[session.Ref] = session
	}

	refs := make([]string, 0, len(byRef)+len(liveRefs))
	for ref := range byRef {
		refs = append(refs, ref)
	}
	for ref := range liveRefs {
		if _, recorded := byRef[ref]; !recorded {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	if len(refs) == 0 {
		_, err := fmt.Fprintf(out, "no agent sessions on %s\n", boxName)
		return err
	}

	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tPROVIDER\tTREE\tSTATE\tAGE")
	for _, ref := range refs {
		session, recorded := byRef[ref]
		_, running := liveRefs[ref]
		state := "unrecorded"
		since := liveRefs[ref].CreatedAt
		if recorded {
			state = sessionState(session, running)
			since = session.Entry.CreatedAt
			if session.Entry.Result == "" {
				since = session.Entry.UpdatedAt
			}
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", ref, display(string(session.Provider)), display(session.Tree), state, age(since, now))
	}
	return writer.Flush()
}

// age renders how long ago something happened, at the coarsest scale that still
// answers the question. A session with no recorded time reports no age.
func age(since, now time.Time) string {
	if since.IsZero() {
		return "-"
	}
	elapsed := now.Sub(since)
	if elapsed < 0 {
		elapsed = 0
	}
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(elapsed.Hours()), int(elapsed.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(elapsed.Hours())/24, int(elapsed.Hours())%24)
	}
}

// display renders an absent value as a dash, so a column is never blank and
// cannot be mistaken for a column that failed to print.
func display(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
