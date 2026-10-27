package localapi

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Shared by the unix and Windows test builds: this file has no build tag.
func validSnapshot() Snapshot {
	return Snapshot{
		Schema: SnapshotSchemaV1, Generation: 7, ObservedAt: time.Date(2026, 8, 4, 1, 2, 3, 0, time.UTC), DaemonState: "ready", DaemonVersion: "dev",
		Machines: []MachineStatus{{ID: "machine_1", Alias: "studio", Eligible: true, RuntimeState: "ready", Generation: 4, SelectedPath: "relay", RelayRegion: "bom", TransferReadiness: "ready", PreviewReadiness: "degraded", SSHReadiness: "unavailable", NATMappingIPv4: "unknown", NATMappingIPv6: "unknown", CaptivePortal: "unknown", PMTU: "unknown", RouterProtocol: "unknown", RouterMapping: "unknown", MappingLifetime: "unknown", UpdateHealth: "unknown", Health: []HealthItem{{Code: "ssh_unavailable", Severity: "warning", Title: "SSH is unavailable", Recovery: "Run pb ssh doctor", ETag: "etag_1"}}}},
	}
}

// A snapshot of the most machines the daemon accepts, each with a health item, fits the snapshot bound
// (Lane X audit A1: the 1 MiB request bound broke pb status near 1,500 machines).
func TestASnapshotOfTheMostMachinesFits(t *testing.T) {
	snapshot := validSnapshot()
	template := snapshot.Machines[0]
	template.Health = []HealthItem{{Code: "ssh_host_keys_unpublished", Severity: "warning", Title: "The machine's SSH host keys are not published", Recovery: "A newly set up machine publishes them within a minute or two. If this stays, on the machine start an SSH server (sshd) if none runs; if sshd runs, its host keys may be identical to another machine's (a cloned VM or image): remove /etc/ssh/ssh_host_*, run ssh-keygen -A and restart sshd", ETag: "ssh_host_keys_unpublished"}}
	snapshot.Machines = make([]MachineStatus, maxMachines)
	for i := range snapshot.Machines {
		machine := template
		machine.ID = fmt.Sprintf("machine_%08d-0000-0000-0000-000000000000", i)
		machine.Alias = fmt.Sprintf("build-host-%05d", i)
		snapshot.Machines[i] = machine
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > maxSnapshotBytes {
		t.Fatalf("snapshot of %d machines is %d bytes, bound %d (err %v)", maxMachines, len(encoded), maxSnapshotBytes, err)
	}
}
