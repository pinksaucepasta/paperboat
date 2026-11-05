//go:build darwin || linux

package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// The host closes a native terminal's connection on purpose when its carrier hands the network over. The attachment
// must outlive that close for the grace period, or the client's resume (which proves the attachment's secret, held
// only by the PTY owner) is refused and it falls back to a full grant: seconds instead of one round trip.
func TestClosedNativeTerminalStreamKeepsItsAttachmentForTheResumeGrace(t *testing.T) {
	previous := terminalResumeGrace
	terminalResumeGrace = 300 * time.Millisecond
	t.Cleanup(func() { terminalResumeGrace = previous })
	dispatcher, root := execDispatcher(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	created, err := dispatcher.config.Sessions.Create(ctx, session.CreateRequest{ID: "ses_grace", Name: "grace", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "exec cat"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	auth := Authorization{AccountID: "account_owner", ClientID: "cli_owner", SessionID: created.ID, TerminalRole: TerminalRoleOwner, ResourceID: "grant_1"}
	payload, _ := json.Marshal(map[string]any{"action": "attach", "session_id": created.ID, "attachment_id": "att_grace", "from_sequence": 0})
	open := func() *terminalOutputStream {
		t.Helper()
		outcome := dispatcher.Handle(ctx, auth, "terminal.v1", payload)
		if outcome.ErrorCode != "" {
			t.Fatal("attach failed", outcome.ErrorCode)
		}
		stream, opened, err := dispatcher.OpenStream(ctx, auth, "terminal.v1", payload, outcome, false)
		if err != nil || !opened {
			t.Fatal("stream unavailable", err)
		}
		return stream.(*terminalOutputStream)
	}
	resume := func(secret []byte) error {
		_, err := dispatcher.config.Sessions.ResumeAttachment(created.ID, session.ResumeRequest{AttachmentID: "att_grace", AccountID: "account_owner", ClientID: "cli_owner", Resource: "grant_1", Secret: secret})
		return err
	}
	stream := open()
	secret := stream.resumeSecret
	if len(secret) == 0 {
		t.Fatal("a native owner attachment was issued no resume secret")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := resume(secret); err != nil {
		t.Fatalf("resume right after the host closed the connection: %v", err)
	}
	// The resumed attachment is the new connection's: the old connection's grace timer must not detach it.
	time.Sleep(3 * terminalResumeGrace)
	if writers, err := dispatcher.config.Sessions.ActiveWriters(created.ID); err != nil || len(writers) != 1 {
		t.Fatalf("the grace timer of the closed connection detached the resumed attachment: %v %v", writers, err)
	}
	// An attachment nobody resumes is detached when the grace ends.
	lost := open()
	lostSecret := lost.resumeSecret
	if err := lost.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * terminalResumeGrace)
	if err := resume(lostSecret); err == nil {
		t.Fatal("an unresumed attachment outlived the grace")
	}
}
