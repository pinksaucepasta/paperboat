package localdaemon

import (
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// X-42: a support bundle holds nothing that identifies other machines of the account (ids, names, owners, tags, SSH
// users, home directories); how this machine reaches them (path, relay region; F21-06) and the daemon's health stay.
func TestBugreportStatusHoldsNoOtherMachines(t *testing.T) {
	roots := []string{"/home/alice", "/Users/alice", `C:\Users\alice`}
	machines := make([]localapi.MachineStatus, len(roots))
	for i, root := range roots {
		machines[i] = localapi.MachineStatus{ID: "machine_" + string(rune('a'+i)), Alias: "studio-" + string(rune('a'+i)), OwnerAccount: "acct_owner", Tags: []string{"tag:prod"}, WorkspaceRoot: root, SSHUser: "alice", SSHHostKeys: []string{"ssh-ed25519 AAAAkey"}, Platform: "linux", RuntimeState: "ready", SelectedPath: "relay", RelayRegion: "fra"}
	}
	snapshot := localapi.Snapshot{DaemonState: "ready", DaemonVersion: "dev-x", Health: []localapi.HealthItem{{Code: "relay_unreachable"}}, Machines: machines}
	status, err := bugreportStatus(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	text := string(status)
	for _, leaked := range []string{"alice", "/home/", "/Users/", "studio-", "machine_a", "acct_owner", "tag:prod", "AAAAkey", "linux"} {
		if strings.Contains(text, leaked) {
			t.Errorf("the bundle status holds %q: %s", leaked, text)
		}
	}
	for _, kept := range []string{`"daemon_state":"ready"`, `"daemon_version":"dev-x"`, "relay_unreachable", `"selected_path":"relay"`, `"relay_region":"fra"`} {
		if !strings.Contains(text, kept) {
			t.Errorf("the bundle status lost %q: %s", kept, text)
		}
	}
	if len(snapshot.Machines) != 3 || snapshot.Machines[0].WorkspaceRoot != roots[0] {
		t.Fatal("the daemon's own snapshot was changed")
	}
}
