//go:build linux || darwin

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/shared/terminalprotocol"
)

type stallingSessions struct {
	session.Service
	id      string
	release chan struct{}
}

func (s stallingSessions) SnapshotAtGeneration(id string, generation uint64) (session.Snapshot, error) {
	if id == s.id {
		<-s.release
	}
	return s.Service.SnapshotAtGeneration(id, generation)
}

// One request reads exactly the sessions the signed hash names, and nobody but the machine's owner credential may send it.
func TestTerminalSnapshotsReadOnlyTheSignedSessions(t *testing.T) {
	dispatcher, _ := execDispatcher(t)
	ids := []string{"session_absent_1", "session_absent_2"}
	owner := Authorization{ClientID: "control-plane", SessionID: ids[0], TerminalRole: TerminalRoleOwner, RequestHash: terminalprotocol.SnapshotsHash(ids)}
	ask := func(authorization Authorization, ids []string) (string, []terminalSnapshotResult) {
		payload, _ := json.Marshal(map[string]any{"action": "snapshots", "session_ids": ids})
		outcome := dispatcher.Handle(context.Background(), authorization, "terminal.v1", payload)
		var answer struct {
			Sessions []terminalSnapshotResult `json:"sessions"`
		}
		if outcome.ErrorCode == "" {
			if err := json.Unmarshal(outcome.Result, &answer); err != nil {
				t.Fatal(err)
			}
		}
		return outcome.ErrorCode, answer.Sessions
	}
	code, sessions := ask(owner, ids)
	if code != "" || len(sessions) != 2 || sessions[0].SessionID != ids[0] || sessions[1].Error != "not_found_or_forbidden" {
		t.Fatalf("signed sessions: %s %+v", code, sessions)
	}
	stalled := make(chan struct{})
	defer close(stalled)
	dispatcher.config.Sessions = stallingSessions{dispatcher.config.Sessions, ids[1], stalled}
	if code, sessions = ask(owner, ids); code != "" || len(sessions) != 2 || sessions[0].Error != "not_found_or_forbidden" || sessions[1].Error != "deadline_exceeded" {
		t.Fatalf("a stalled session withheld the others: %s %+v", code, sessions)
	}
	many := make([]string, terminalprotocol.MaxSnapshotSessions+1)
	for i := range many {
		many[i] = fmt.Sprintf("session_%d", i)
	}
	with := func(change func(*Authorization)) Authorization {
		changed := owner
		change(&changed)
		return changed
	}
	for name, refused := range map[string]struct {
		authorization Authorization
		ids           []string
	}{
		"another session":  {owner, []string{ids[0], "session_other"}},
		"one more session": {owner, append([]string{ids[0]}, ids...)},
		"no hash":          {with(func(a *Authorization) { a.RequestHash = "" }), ids},
		"another first":    {with(func(a *Authorization) { a.SessionID = "session_other" }), ids},
		"no session":       {with(func(a *Authorization) { a.RequestHash = terminalprotocol.SnapshotsHash(nil) }), nil},
		"too many":         {with(func(a *Authorization) { a.SessionID, a.RequestHash = many[0], terminalprotocol.SnapshotsHash(many) }), many},
		"viewer":           {with(func(a *Authorization) { a.TerminalRole = TerminalRoleViewer }), ids},
		"interactive":      {with(func(a *Authorization) { a.TerminalRole = TerminalRoleInteractive }), ids},
		"browser":          {with(func(a *Authorization) { a.BrowserTerminal = true }), ids},
	} {
		if code, _ := ask(refused.authorization, refused.ids); code != "not_found_or_forbidden" {
			t.Fatalf("%s: %q", name, code)
		}
	}
}
