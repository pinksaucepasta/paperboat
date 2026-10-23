//go:build darwin || linux

package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcastserver"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// Lane Q / Lane W: 50 console viewers polled /sharing every 4 s. A browser terminal stream now
// reports the session's participants itself: the current list first, then whenever someone joins,
// leaves or changes role, at most every participantsInterval.
func TestBrowserTerminalStreamReportsParticipantChanges(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Shutdown(context.Background())
	created, err := sessions.Create(context.Background(), session.CreateRequest{Name: "people", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "exec sleep 60"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	stream := &terminalOutputStream{manager: sessions, sessionID: created.ID, keyUpdates: make(chan browserbroadcastserver.Epoch)}
	read := func() []session.Participant {
		t.Helper()
		var event *StreamEvent
		if err := stream.participantsEvent(); !errors.As(err, &event) {
			t.Fatalf("participants event = %v", err)
		}
		var payload struct {
			Event        string                `json:"event"`
			SessionID    string                `json:"session_id"`
			Participants []session.Participant `json:"participants"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Event != "participants" || payload.SessionID != created.ID {
			t.Fatalf("payload %s err=%v", event.Payload, err)
		}
		return payload.Participants
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("initial participants = %#v", got)
	}
	if err := stream.participantsEvent(); err != nil {
		t.Fatalf("an unchanged list was reported again: %v", err)
	}
	viewer := session.Participant{AttachmentID: "att_viewer", AccountID: "acct_teammate", ClientID: "client_teammate", Role: "viewer", ConnectedAt: time.Now().UTC()}
	if _, err := sessions.AttachLiveParticipantAtGeneration(created.ID, viewer, created.Generation); err != nil {
		t.Fatal(err)
	}
	time.Sleep(participantsInterval)
	got := read()
	if len(got) != 1 || got[0].AttachmentID != "att_viewer" || got[0].AccountID != "acct_teammate" || got[0].Role != "viewer" {
		t.Fatalf("participants after a viewer joined = %#v", got)
	}
	time.Sleep(participantsInterval)
	if err := stream.participantsEvent(); err != nil {
		t.Fatalf("an unchanged list was reported again: %v", err)
	}
}
