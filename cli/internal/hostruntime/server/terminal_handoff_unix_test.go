//go:build darwin || linux

package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

func TestOldTerminalStreamCannotReadOrDetachReplacement(t *testing.T) {
	dispatcher, root := execDispatcher(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	created, err := dispatcher.config.Sessions.Create(ctx, session.CreateRequest{ID: "ses_handoff", Name: "handoff", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "exec cat"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	auth := Authorization{AccountID: "account_owner", SourceMachineID: "machine_sender", ClientID: "cli_owner", SessionID: created.ID, TerminalRole: TerminalRoleOwner, TerminalGeneration: created.Generation}
	payload, _ := json.Marshal(map[string]any{"action": "attach", "session_id": created.ID, "attachment_id": "att_handoff", "from_sequence": 0})
	attach := func() *terminalOutputStream {
		t.Helper()
		outcome := dispatcher.Handle(ctx, auth, "terminal.v1", payload)
		if outcome.ErrorCode != "" {
			t.Fatal("real terminal attach failed", outcome.ErrorCode)
		}
		stream, opened, err := dispatcher.OpenStream(ctx, auth, "terminal.v1", payload, outcome, false)
		if err != nil || !opened {
			t.Fatal("real terminal stream unavailable", err)
		}
		return stream.(*terminalOutputStream)
	}
	old := attach()
	next := attach()
	defer next.Close()
	if old.attachmentLease == 0 || old.attachmentLease == next.attachmentLease {
		t.Fatal("attachment takeover reused its transport lease")
	}
	// An old in-flight poll must not advance the new replay queue, even if its
	// cursor was ahead of what the client actually received before switching.
	if _, err := dispatcher.config.Sessions.WaitNextAttachment(ctx, created.ID, "att_handoff", ^uint64(0), old.attachmentLease); !errors.Is(err, session.ErrAttachmentReplaced) {
		t.Fatal("old reader retained the replacement queue", err)
	}
	// The old link ends marked as handed over, so its client adopts the newer link instead of ending the terminal.
	for frames := 0; ; frames++ {
		_, err := old.Next(ctx)
		if err == nil && frames < 64 {
			continue // the old attachment's checkpoint
		}
		var end *StreamEnd
		if !errors.As(err, &end) || string(end.Payload) != `{"event":"terminal_handed_over","session_id":"ses_handoff"}` {
			t.Fatal("old stream did not end as handed over", err)
		}
		break
	}
	if err := old.Close(); err != nil {
		t.Fatal("old stream cleanup should be a no-op", err)
	}
	// The old link's own detach arrives after the probe adopted the attachment (the network came back at once, so the
	// old link still delivers): it names the old lease and must leave the newer attach streaming. A detach without a
	// lease is refused. Both the browser (generation-bound) and the owner path check it.
	browserAuth := auth
	browserAuth.BrowserTerminal, browserAuth.BrowserAttachmentID = true, "att_handoff"
	detach := func(authorization Authorization, lease uint64) string {
		control, _ := json.Marshal(map[string]any{"session_id": created.ID, "attachment_id": "att_handoff", "attachment_lease": lease})
		return dispatcher.HandleControl(ctx, authorization, protocol.Frame{Type: "detach", Payload: control}).ErrorCode
	}
	for _, authorization := range []Authorization{auth, browserAuth} {
		if code := detach(authorization, old.attachmentLease); code != "" {
			t.Fatal("old link's detach failed", code)
		}
		if code := detach(authorization, 0); code == "" {
			t.Fatal("a detach without a lease was accepted")
		}
	}
	writers, err := dispatcher.config.Sessions.ActiveWriters(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if machines := filetransfer.EligibleWriterMachines(writers); len(machines) != 1 || machines[0] != auth.SourceMachineID {
		t.Fatal("old cleanup removed the replacement file recipient")
	}
	if _, err := dispatcher.HandleTerminalInput(ctx, auth, created.ID, "att_handoff", created.Generation, 1, []byte("after\n")); err != nil {
		t.Fatal("old stream cleanup detached the replacement", err)
	}
	event, err := dispatcher.config.Sessions.WaitNextAttachment(ctx, created.ID, "att_handoff", 0, next.attachmentLease)
	if err != nil {
		t.Fatal("replacement lost real PTY output", err)
	}
	defer event.Release()
	if len(event.Data) == 0 {
		t.Fatal("replacement received no PTY output")
	}
	if code := detach(auth, next.attachmentLease); code != "" {
		t.Fatal("the current attach's detach failed", code)
	}
	if _, err := dispatcher.config.Sessions.WaitNextAttachment(ctx, created.ID, "att_handoff", 0, next.attachmentLease); !errors.Is(err, session.ErrAttachmentUnknown) {
		t.Fatal("a detach naming the current lease left the attachment", err)
	}
}
