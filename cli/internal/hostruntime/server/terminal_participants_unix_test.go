//go:build darwin || linux

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcastserver"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
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

// A browser that fell behind asks its live stream for a fresh screen: the stream answers with a complete
// checkpoint of the current screen at the current boundary, without a new attachment.
func TestBrowserTerminalStreamSendsAScreenOnRequest(t *testing.T) {
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
	created, err := sessions.Create(context.Background(), session.CreateRequest{Name: "screen", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "printf screen-marker; exec sleep 60"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	browser := session.Participant{AttachmentID: "att_browser", AccountID: "acct_owner", ClientID: "client_browser", Role: "owner", ConnectedAt: time.Now().UTC(), Browser: true}
	if _, err := sessions.AttachLiveParticipantAtGeneration(created.ID, browser, created.Generation); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if snapshot, _ := sessions.Snapshot(created.ID); snapshot.LatestSequence >= uint64(len("screen-marker")) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	requests := make(chan struct{}, 1)
	stream := &terminalOutputStream{manager: sessions, sessionID: created.ID, attachmentID: browser.AttachmentID, checkpointSent: true, keyUpdates: make(chan browserbroadcastserver.Epoch), screenRequests: requests, accountID: browser.AccountID, clientID: browser.ClientID, generation: created.Generation}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var event *StreamEvent
	if _, err := stream.Next(ctx); !errors.As(err, &event) { // the first participants report
		t.Fatalf("first frame: %v", err)
	}
	requests <- struct{}{}
	frame, err := stream.Next(ctx)
	if err != nil || frame.Channel != protocol.TerminalScreenCheckpoint || len(frame.Data) < 2 || frame.Data[0] != 1 || !bytes.Contains(frame.Data[1:], []byte("screen-marker")) || frame.StartSequence < uint64(len("screen-marker")) {
		t.Fatalf("screen on request: channel=%d start=%d err=%v", frame.Channel, frame.StartSequence, err)
	}
}

type countingSnapshots struct {
	session.Service
	calls atomic.Int64
}

func (c *countingSnapshots) Snapshot(id string) (session.Snapshot, error) {
	c.calls.Add(1)
	return c.Service.Snapshot(id)
}

func (c *countingSnapshots) SnapshotAtGeneration(id string, generation uint64) (session.Snapshot, error) {
	c.calls.Add(1)
	return c.Service.SnapshotAtGeneration(id, generation)
}

// A flood must not cost a session snapshot per output event: each is an owner round trip that takes the output
// lock, and one per event held terminal output to about one event a second on a device.
func TestFloodingStreamChecksTheSessionAtMostOncePerInterval(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 2, AttachmentBytes: 1 << 20, HistoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Shutdown(context.Background())
	created, err := sessions.Create(context.Background(), session.CreateRequest{Name: "flood", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "head -c 2000000 /dev/zero | tr '\\\\0' x; exec sleep 30"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Attach(created.ID, "att_flood", 0); err != nil {
		t.Fatal(err)
	}
	counting := &countingSnapshots{Service: sessions}
	stream := &terminalOutputStream{manager: counting, sessionID: created.ID, attachmentID: "att_flood", checkpointSent: true}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	events := 0
	for received := 0; received < 2_000_000; events++ {
		frame, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("after %d bytes: %v", received, err)
		}
		received += len(frame.Data)
		if frame.Release != nil {
			frame.Release()
		}
	}
	if calls := counting.calls.Load(); calls > 3 {
		t.Fatalf("%d snapshots for %d output events", calls, events)
	}
}
