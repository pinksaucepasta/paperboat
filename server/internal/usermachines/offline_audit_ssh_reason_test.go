package usermachines

import (
	"testing"
	"time"
)

// A host refuses a stream when macOS Remote Login does not allow the managed user and audits the bare reason.
func TestSSHRefusalAuditAcceptsLoginNotAllowedWithoutDetail(t *testing.T) {
	event := func(reason string) OfflineAuditEvent {
		return OfflineAuditEvent{EventID: "ssh_x", Kind: "ssh_session_refused", PeerEndpointID: "cli_x", PeerAccountID: "acct_x", OccurredAt: time.Now().UTC(), Origin: "online", Reason: reason}
	}
	if !validOfflineAuditEvent(event("login_not_allowed"), time.Now().UTC()) {
		t.Fatal("login_not_allowed refusal was rejected")
	}
	if validOfflineAuditEvent(event("login_not_allowed:pbmac2"), time.Now().UTC()) {
		t.Fatal("a refusal carrying an OS user name was accepted: the audit records the bare reason")
	}
}
