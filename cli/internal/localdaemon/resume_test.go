package localdaemon

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func ownerTarget(secret []byte) *resolver.TerminalTarget {
	return &resolver.TerminalTarget{SessionID: "ses_1", InputAttachmentID: "att_1", ResumeSecret: secret, Auth: resolver.AuthTarget{Token: "credential", ResourceID: "session_old"}}
}

// A redial after a clean close re-attaches with the secret the owner's last attach brought, once, under the access
// session and attachment that attach used; with none held the request is answered resume_unavailable.
func TestResumeUsesTheOwnersSecretOnce(t *testing.T) {
	owners := &inputOwners{entries: make(map[inputOwnerKey]inputOwnerEntry)}
	previous := ownerTarget(bytes.Repeat([]byte{7}, 32))
	now := time.Now()
	owners.adopt("owner_1", "machine_1", previous, now)
	previousOwners := terminalInputOwners
	terminalInputOwners = owners
	t.Cleanup(func() { terminalInputOwners = previousOwners })
	next := &resolver.TerminalTarget{SessionID: "ses_1"}
	if !applyResume(next, "machine_1", localapi.PeerTerminalPayload{InputOwner: "owner_1", SessionID: "ses_1"}, now) {
		t.Fatal("a held secret was not used")
	}
	if !next.Resume || !bytes.Equal(next.ResumeSecret, bytes.Repeat([]byte{7}, 32)) || next.Auth.Token != "" || next.Auth.ResourceID != "session_old" || next.InputAttachmentID != "att_1" {
		t.Fatalf("resume target = %+v", next)
	}
	again := &resolver.TerminalTarget{SessionID: "ses_1"}
	if applyResume(again, "machine_1", localapi.PeerTerminalPayload{InputOwner: "owner_1", SessionID: "ses_1"}, now) || again.Resume {
		t.Fatal("a single-use secret was used twice")
	}
	var unavailable localapi.ResumeUnavailableError
	if !errors.As(error(localapi.ResumeUnavailableError{}), &unavailable) || unavailable.LocalAPICode() != "resume_unavailable" {
		t.Fatalf("code = %q", unavailable.LocalAPICode())
	}
	if applyResume(&resolver.TerminalTarget{}, "machine_1", localapi.PeerTerminalPayload{InputOwner: "other", SessionID: "ses_1"}, now) {
		t.Fatal("another owner's terminal was resumed")
	}
}

// An open stream's entry survives hours so a long-held terminal can resume; an ended one lives a few minutes.
func TestResumeSecretLivesWithItsStream(t *testing.T) {
	owners := &inputOwners{entries: make(map[inputOwnerKey]inputOwnerEntry)}
	target := ownerTarget(bytes.Repeat([]byte{1}, 32))
	start := time.Now()
	owners.adopt("owner_1", "machine_1", target, start)
	if _, _, _, ok := owners.takeResume("owner_1", "machine_1", "ses_1", start.Add(5*time.Hour)); !ok {
		t.Fatal("the secret of a stream open for hours was lost")
	}
	target.ResumeSecret = bytes.Repeat([]byte{1}, 32)
	owners.finish("owner_1", "machine_1", target, start.Add(5*time.Hour))
	if _, _, _, ok := owners.takeResume("owner_1", "machine_1", "ses_1", start.Add(5*time.Hour+inputOwnerTTL+time.Second)); ok {
		t.Fatal("the secret outlived its ended stream")
	}
}
