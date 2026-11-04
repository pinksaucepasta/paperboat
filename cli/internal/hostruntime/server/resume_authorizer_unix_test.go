//go:build darwin || linux

package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type testResumeConn struct{ account, endpoint, session, resource string }

func (c testResumeConn) ResumePeer() (string, string, string, string, bool) {
	return c.account, c.endpoint, c.session, c.resource, false
}

// A connection admitted to resume runs nothing, not even a create or close, until it proves the
// attachment's secret; then it is that attachment's owner connection.
func TestResumeConnectionRunsNothingBeforeItProvesTheSecret(t *testing.T) {
	dispatcher, root := execDispatcher(t)
	sessions := dispatcher.config.Sessions
	created, err := sessions.Create(context.Background(), session.CreateRequest{Name: "resume", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "read line"}, Env: []string{"PATH=/usr/bin:/bin"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := sessions.AttachParticipant(created.ID, session.Participant{AttachmentID: "att_1", AccountID: "acct_1", ClientID: "cli_1", Role: "owner", ConnectedAt: time.Now()}, 0)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := sessions.IssueResume(created.ID, "att_1", attached.AttachmentLease, "grant_1")
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := newResumeAuthorizer(testResumeConn{"acct_1", "cli_1", created.ID, "grant_1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorizer.Authorize(context.Background(), protocol.Frame{Capability: "exec.v1"}); err == nil {
		t.Fatal("a resume connection was authorized beyond its terminal")
	}
	authorization, err := authorizer.Authorize(context.Background(), protocol.Frame{Capability: "terminal.v1"})
	if err != nil {
		t.Fatal(err)
	}
	handle := func(request map[string]any) string {
		request["session_id"] = created.ID
		payload, _ := json.Marshal(request)
		return dispatcher.Handle(context.Background(), authorization, "terminal.v1", payload).ErrorCode
	}
	wrong := append([]byte(nil), secret...)
	wrong[0] ^= 1
	for _, request := range []map[string]any{
		{"action": "close"},
		{"action": "create", "existing_snapshot": true},
		{"action": "attach", "attachment_id": "att_1"},
		{"action": "attach", "attachment_id": "att_1", "resume_secret": wrong},
	} {
		if code := handle(request); code != "not_found_or_forbidden" {
			t.Fatalf("%v before the proof: code=%q", request["action"], code)
		}
	}
	if snapshot, err := sessions.Snapshot(created.ID); err != nil || snapshot.State != session.Running {
		t.Fatalf("session changed before the proof: %#v %v", snapshot, err)
	}
	if code := handle(map[string]any{"action": "attach", "attachment_id": "att_1", "resume_secret": secret}); code != "" {
		t.Fatalf("resume with the secret: code=%q", code)
	}
	if code := handle(map[string]any{"action": "snapshot"}); code != "" {
		t.Fatalf("snapshot after the resume: code=%q", code)
	}
}
