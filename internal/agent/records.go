package agent

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"devbox/internal/box"
	"devbox/internal/record"
)

// KindSession labels the durable note for an agent session in the record store,
// so an unresolved session can be told apart from the cloud mutations that share
// the store. The kind lives here because the record store is the shared contract
// and only this slice starts sessions.
const KindSession = "agent"

// sessionRecord is one durable session note, decoded from the store.
type sessionRecord struct {
	Entry    record.Record
	Ref      string
	Provider Provider
	Tree     string
	Dir      string
	Session  string
}

// unresolved reports whether the record still blocks a start for its provider.
func (s sessionRecord) unresolved() bool {
	return s.Entry.State == record.StatePending || s.Entry.State == record.StateUnknown
}

// sessionArgs renders the durable argument vector for a start. The pairs are self
// describing because this vector is what an operator reads when a start was left
// unresolved and the cloud state has to be worked out by hand.
func sessionArgs(request request, ref, command string) []string {
	return []string{
		"id=" + ref,
		"session=" + sessionName(ref),
		"provider=" + string(request.Provider),
		"tree=" + request.Tree,
		"dir=" + request.Dir,
		"command=" + command,
	}
}

// parseSessionRecord decodes the argument vector a start wrote. A record without
// a usable id is not addressable, so it is reported as unreadable rather than
// shown as a session with no name.
func parseSessionRecord(entry record.Record) (sessionRecord, bool) {
	session := sessionRecord{Entry: entry}
	for _, arg := range entry.Args {
		key, value, found := strings.Cut(arg, "=")
		if !found {
			continue
		}
		switch key {
		case "id":
			session.Ref = value
		case "session":
			session.Session = value
		case "provider":
			session.Provider = Provider(value)
		case "tree":
			session.Tree = value
		case "dir":
			session.Dir = value
		}
	}
	if session.Ref == "" {
		return sessionRecord{}, false
	}
	return session, true
}

// sessionRecords returns every session note for a box, oldest first. A record devbox
// cannot decode is reported, never silently dropped: it may be the only evidence
// that a session exists.
func sessionRecords(store *record.Store, boxName box.Name) ([]sessionRecord, error) {
	if store == nil {
		return nil, errors.New("session state needs a record store")
	}
	entries, err := store.All()
	if err != nil {
		return nil, err
	}
	var out []sessionRecord
	for _, entry := range entries {
		if entry.Kind != KindSession || entry.Box != string(boxName) {
			continue
		}
		session, ok := parseSessionRecord(entry)
		if !ok {
			return nil, fmt.Errorf("record %s names no session id; inspect it before starting another session on %s", entry.ID, boxName)
		}
		out = append(out, session)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Entry.CreatedAt.Before(out[j].Entry.CreatedAt) })
	return out, nil
}

// findSession returns the session note for one id.
func findSession(store *record.Store, boxName box.Name, ref string) (sessionRecord, error) {
	records, err := sessionRecords(store, boxName)
	if err != nil {
		return sessionRecord{}, err
	}
	for _, session := range records {
		if session.Ref == ref {
			return session, nil
		}
	}
	return sessionRecord{}, fmt.Errorf("box %s has no record of session %s: devbox only stops or reconciles a session it started", boxName, ref)
}

// blockingSessions returns the notes that stop a new start: a start whose outcome
// devbox never established. A note that names no provider blocks every provider,
// because devbox cannot show it belongs to someone else.
func blockingSessions(store *record.Store, boxName box.Name, provider Provider) ([]sessionRecord, error) {
	records, err := sessionRecords(store, boxName)
	if err != nil {
		return nil, err
	}
	var out []sessionRecord
	for _, session := range records {
		if !session.unresolved() {
			continue
		}
		if session.Provider != "" && session.Provider != provider {
			continue
		}
		out = append(out, session)
	}
	return out, nil
}

// refusal builds the error that stops a start, naming every blocking session and
// the exact command that clears it.
func refusal(store *record.Store, request request) error {
	blocking, err := blockingSessions(store, request.Box, request.Provider)
	if err != nil {
		return err
	}
	if len(blocking) == 0 {
		return nil
	}
	now := time.Now().UTC()
	lines := []string{fmt.Sprintf("refusing to start a %s session on %s: %d unresolved session(s) already recorded", request.Provider, request.Box, len(blocking))}
	for _, session := range blocking {
		lines = append(lines, fmt.Sprintf("  %s (%s, %s, %s old): %s",
			session.Ref, display(string(session.Provider)), display(session.Tree), age(session.Entry.CreatedAt, now), unsettled(session.Entry)))
		lines = append(lines, fmt.Sprintf("  clear it with: devbox agent reconcile %s %s", request.Box, session.Ref))
	}
	return errors.New(strings.Join(lines, "\n"))
}

// unsettled explains why a record is unresolved, preferring the note devbox wrote
// when the outcome was lost.
func unsettled(entry record.Record) string {
	if entry.Detail != "" {
		return fmt.Sprintf("%s: %s", entry.State, entry.Detail)
	}
	return fmt.Sprintf("%s since %s", entry.State, entry.CreatedAt.UTC().Format(time.RFC3339))
}
