//go:build windows

package managedssh

import (
	"strings"
	"testing"
)

// X-28: the endpoint is per Windows user. One host-wide pipe let a second OS user's daemon (or a leftover one of
// the previous user) take it, and every other user's managed SSH failed to start with "access denied".
func TestInstalledAgentSocketIsPerUserNamedPipe(t *testing.T) {
	got, err := InstalledAgentSocket(`C:\Users\Pujan\AppData\Local\paperboat\run`)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := currentManagedSSHSID()
	if err != nil {
		t.Fatal(err)
	}
	if !validWindowsAgentPipe(got) || strings.Contains(got, sid) || strings.EqualFold(got, `\\.\pipe\Paperboat-SSH-Agent`) {
		t.Fatalf("InstalledAgentSocket() = %q, want an opaque per-user pipe", got)
	}
	if again, _ := InstalledAgentSocket(""); again != got {
		t.Fatalf("not stable: %q then %q", got, again)
	}
}
