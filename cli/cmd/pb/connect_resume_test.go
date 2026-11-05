package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// A redial of a held terminal re-attaches under the access session it already has, with no credential, and leaves the
// descriptor it started from alone (it is the fallback's input too).
func TestResumeConnectInfoReattachesWithoutACredential(t *testing.T) {
	previous := resolver.ConnectInfo{MachineID: "machine_1", Terminal: &resolver.TerminalTarget{SessionID: "ses_1", Auth: resolver.AuthTarget{Token: "operation-credential", ResourceID: "session_old"}, AfterSequence: 3}}
	prepared := false
	got, ok := resumeConnectInfo(previous, func(target *resolver.TerminalTarget) { prepared = true; target.AfterSequence = 9 })
	if !ok || !prepared {
		t.Fatalf("ok=%v prepared=%v", ok, prepared)
	}
	if !got.Terminal.Resume || got.Terminal.Auth.Token != "" || got.Terminal.Auth.ResourceID != "session_old" || got.Terminal.AfterSequence != 9 {
		t.Fatalf("resume descriptor = %+v", got.Terminal)
	}
	if previous.Terminal.Resume || previous.Terminal.Auth.Token != "operation-credential" || previous.Terminal.AfterSequence != 3 {
		t.Fatalf("the descriptor it came from was changed: %+v", previous.Terminal)
	}
	for _, unusable := range []resolver.ConnectInfo{{}, {Terminal: &resolver.TerminalTarget{}}, {Terminal: &resolver.TerminalTarget{Auth: resolver.AuthTarget{ResourceID: "session_old", Offline: true}}}} {
		if _, ok := resumeConnectInfo(unusable, func(*resolver.TerminalTarget) {}); ok {
			t.Fatalf("a descriptor with nothing to resume under was resumed: %+v", unusable)
		}
	}
}
