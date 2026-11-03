//go:build darwin || linux

package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

func TestOldTerminalStreamCannotReadOrDetachReplacement(t *testing.T) {
	dispatcher, root := execDispatcher(t)
	dispatcher.config.Writers = filetransfer.NewWriterRegistry()
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
	if _, err := dispatcher.config.Sessions.WaitNextAttachment(ctx, created.ID, "att_handoff", ^uint64(0), old.attachmentLease); !errors.Is(err, session.ErrAttachmentUnknown) {
		t.Fatal("old reader retained the replacement queue", err)
	}
	if err := old.Close(); err != nil {
		t.Fatal("old stream cleanup should be a no-op", err)
	}
	if machines := dispatcher.config.Writers.EligibleMachines(created.ID); len(machines) != 1 || machines[0] != auth.SourceMachineID {
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
}
