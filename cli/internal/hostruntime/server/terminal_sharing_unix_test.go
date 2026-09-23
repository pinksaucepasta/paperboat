//go:build darwin || linux

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

func TestSharedTerminalRolesAndAttachmentIdentity(t *testing.T) {
	dispatcher, root := execDispatcher(t)
	dispatcher.config.RecordTerminalJoin = func(context.Context, TerminalJoin) error { return nil }
	created, err := dispatcher.config.Sessions.Create(context.Background(), session.CreateRequest{ID: "ses_shared", Name: "shared", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "cat"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	viewer := Authorization{AccountID: "acc_view", ClientID: "cli_view", SessionID: created.ID, TerminalRole: TerminalRoleViewer, TerminalGeneration: created.Generation}
	attach, _ := json.Marshal(map[string]any{"action": "attach", "session_id": created.ID, "from_sequence": 0})
	outcome := dispatcher.Handle(context.Background(), viewer, "terminal.v1", attach)
	if outcome.ErrorCode != "" {
		t.Fatalf("attach=%#v", outcome)
	}
	var response terminalAttachResponse
	if err := json.Unmarshal(outcome.Result, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Session.Snapshot.Participants) != 1 || response.Session.Snapshot.Participants[0].Role != "viewer" || response.Session.Snapshot.Participants[0].AccountID != "acc_view" {
		t.Fatalf("participants=%#v", response.Session.Snapshot.Participants)
	}
	if _, err := dispatcher.HandleTerminalInput(context.Background(), viewer, created.ID, response.AttachmentID, created.Generation, 1, []byte("denied")); err == nil {
		t.Fatal("viewer input accepted")
	}
	forged := viewer
	forged.ClientID = "cli_forged"
	if err := dispatcher.HandleTerminalACK(context.Background(), forged, created.ID, response.AttachmentID, 0); err == nil {
		t.Fatal("forged attachment ACK accepted")
	}
	resize, _ := json.Marshal(map[string]any{"action": "resize", "session_id": created.ID, "attachment_id": response.AttachmentID, "columns": 90, "rows": 30})
	if got := dispatcher.Handle(context.Background(), viewer, "terminal.v1", resize); got.ErrorCode != "not_found_or_forbidden" {
		t.Fatalf("viewer resize=%#v", got)
	}
	admin, _ := json.Marshal(map[string]any{"action": "clear", "session_id": created.ID})
	interactive := viewer
	interactive.TerminalRole = TerminalRoleInteractive
	if got := dispatcher.Handle(context.Background(), interactive, "terminal.v1", admin); got.ErrorCode != "not_found_or_forbidden" {
		t.Fatalf("interactive admin=%#v", got)
	}
}

func TestSharedAttachZeroStartsAtBoundedRetainedTailAfterOverflow(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, HistoryBytes: 8, AttachmentBytes: 8, MaxAttachments: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	created, err := manager.Create(context.Background(), session.CreateRequest{ID: "ses_tail", Name: "tail", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "printf 01234567; sleep 0.02; printf 89abcdef; cat"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, snapshotErr := manager.Snapshot(created.ID)
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if snapshot.EarliestSequence > 0 && snapshot.LatestSequence >= 16 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history did not overflow: %#v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
	attached, err := manager.AttachParticipantAtGeneration(created.ID, session.Participant{AttachmentID: "att_tail", AccountID: "acc_2", ClientID: "cli_2", Role: "viewer", ConnectedAt: time.Now().UTC()}, 0, created.Generation)
	if err != nil {
		t.Fatal(err)
	}
	var replay strings.Builder
	for _, event := range attached.Replay.Events {
		replay.Write(event.Data)
	}
	if replay.String() != "cdef" || attached.Replay.ToSequence-attached.Replay.FromSequence > 4 {
		t.Fatalf("replay=%q bounds=%d..%d", replay.String(), attached.Replay.FromSequence, attached.Replay.ToSequence)
	}
}

func TestRestartDetachesSharedGenerationBeforeReplacementOutput(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxAttachments: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	created, err := manager.Create(context.Background(), session.CreateRequest{ID: "ses_restart_shared", Name: "restart", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "sleep .05; printf generation"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	owner := session.Participant{AttachmentID: "att_owner", AccountID: "acc_owner", ClientID: "cli_owner", Role: "owner", ConnectedAt: time.Now().UTC()}
	viewer := session.Participant{AttachmentID: "att_view", AccountID: "acc_view", ClientID: "cli_view", Role: "viewer", ConnectedAt: time.Now().UTC()}
	if _, err = manager.AttachParticipant(created.ID, owner, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.AttachParticipantAtGeneration(created.ID, viewer, 0, created.Generation); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, snapshotErr := manager.Snapshot(created.ID)
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if snapshot.State == session.Exited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not exit: %#v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
	restarted, err := manager.Restart(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.Participants) != 1 || restarted.Participants[0].Role != "owner" {
		t.Fatalf("participants after restart=%#v", restarted.Participants)
	}
	if _, _, err = manager.Next(created.ID, viewer.AttachmentID); err == nil {
		t.Fatal("shared attachment survived restart")
	}
	if _, err = manager.AttachParticipantAtGeneration(created.ID, viewer, 0, created.Generation); err == nil {
		t.Fatal("stale generation attached to replacement")
	}
	viewer.AttachmentID = "att_view_fresh"
	if _, err = manager.AttachParticipantAtGeneration(created.ID, viewer, 0, restarted.Generation); err != nil {
		t.Fatalf("fresh generation attach: %v", err)
	}
}

func TestSharedTerminalJoinAuditFailureDetachesBeforeReplay(t *testing.T) {
	dispatcher, root := execDispatcher(t)
	created, err := dispatcher.config.Sessions.Create(context.Background(), session.CreateRequest{ID: "ses_audit", Name: "audit", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "cat"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	viewer := Authorization{AccountID: "acc_view", ClientID: "cli_view", ResourceID: "access_a", SessionID: created.ID, TerminalRole: TerminalRoleViewer, TerminalGeneration: created.Generation}
	request, _ := json.Marshal(map[string]any{"action": "attach", "session_id": created.ID, "attachment_id": "att_a"})
	for _, record := range []TerminalJoinRecorder{nil, func(ctx context.Context, in TerminalJoin) error {
		if in.AccessSessionID != "access_a" || in.TerminalSessionID != created.ID || in.AttachmentID != "att_a" {
			t.Fatal("lost exact binding")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded audit")
		}
		return context.DeadlineExceeded
	}} {
		dispatcher.config.RecordTerminalJoin = record
		out := dispatcher.Handle(context.Background(), viewer, "terminal.v1", request)
		if out.ErrorCode != "unavailable" || len(out.Result) > 0 {
			t.Fatalf("failed audit returned output: %#v", out)
		}
		snapshot, err := dispatcher.config.Sessions.Snapshot(created.ID)
		if err != nil || len(snapshot.Participants) != 0 {
			t.Fatalf("attachment leaked: %#v err=%v", snapshot.Participants, err)
		}
	}
	calls := 0
	dispatcher.config.RecordTerminalJoin = func(ctx context.Context, in TerminalJoin) error { calls++; return nil }
	out := dispatcher.Handle(context.Background(), viewer, "terminal.v1", request)
	if out.ErrorCode != "" || calls != 1 {
		t.Fatalf("recovery=%#v calls=%d", out, calls)
	}
}
