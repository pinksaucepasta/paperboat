//go:build darwin || linux

package session

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// The resume secret moves only its own attachment, for the same account and client, under the
// grant it was issued for (or a standing rule), once; wrong attempts are bounded and the secret ends
// with the attachment.
func TestResumeSecretIsBoundSingleUseAndBounded(t *testing.T) {
	manager, root, shell := realManager(t)
	created, err := manager.Create(context.Background(), CreateRequest{Name: "resume", Command: shellCommand(shell, root, "read line")})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background(), created.ID)
	owner := Participant{AttachmentID: "att_1", AccountID: "acct_1", ClientID: "client_1", Role: "owner", ConnectedAt: time.Now()}
	attached, err := manager.AttachParticipant(created.ID, owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueResume(created.ID, "att_1", attached.AttachmentLease+1, "grant_1"); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("a stale lease was issued a secret:", err)
	}
	secret, err := manager.IssueResume(created.ID, "att_1", attached.AttachmentLease, "grant_1")
	if err != nil || len(secret) != ResumeSecretSize {
		t.Fatalf("secret=%d err=%v", len(secret), err)
	}
	valid := ResumeRequest{AttachmentID: "att_1", AccountID: "acct_1", ClientID: "client_1", Resource: "grant_1", Secret: secret}
	refuse := func(why string, change func(*ResumeRequest)) {
		t.Helper()
		request := valid
		request.Secret = bytes.Clone(valid.Secret)
		change(&request)
		if _, err := manager.ResumeAttachment(created.ID, request); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: err=%v", why, err)
		}
	}
	refuse("wrong secret", func(r *ResumeRequest) { r.Secret[0] ^= 1 })
	refuse("other client", func(r *ResumeRequest) { r.ClientID = "client_2" })
	// Two failures so far; the third ends the secret, so even the right one no longer works.
	refuse("other account", func(r *ResumeRequest) { r.AccountID = "acct_2" })
	refuse("exhausted", func(*ResumeRequest) {})

	secret, err = manager.IssueResume(created.ID, "att_1", attached.AttachmentLease, "grant_1")
	if err != nil {
		t.Fatal(err)
	}
	valid.Secret = secret
	refuse("other grant", func(r *ResumeRequest) { r.Resource = "grant_2" })
	request := valid
	request.Resource, request.Standing = "machine_host", true
	moved, err := manager.ResumeAttachment(created.ID, request)
	if err != nil || moved.AttachmentLease == attached.AttachmentLease {
		t.Fatalf("resume under a standing rule: lease=%d err=%v", moved.AttachmentLease, err)
	}
	refuse("replay", func(*ResumeRequest) {})

	rotated, err := manager.IssueResume(created.ID, "att_1", moved.AttachmentLease, "grant_1")
	if err != nil || bytes.Equal(rotated, secret) {
		t.Fatalf("rotation reused the secret: err=%v", err)
	}
	if err := manager.DetachAttachment(created.ID, "att_1", moved.AttachmentLease); err != nil {
		t.Fatal(err)
	}
	valid.Secret = rotated
	refuse("detached attachment", func(*ResumeRequest) {})
}
