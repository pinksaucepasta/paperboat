package localdaemon

import (
	"encoding/json"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// bugreportStatus is the daemon status a support bundle holds. Each peer keeps only how this machine reaches it (path,
// relay region, readiness, NAT facts, health; F21-06) and nothing that identifies it or its owner: no id, name, owner,
// tags, platform, SSH user, host keys or home directory (X-42). The fields are copied by name, so a field added to the
// snapshot later stays out of bug reports until it is listed here. The daemon's own snapshot is not changed.
func bugreportStatus(snapshot localapi.Snapshot) ([]byte, error) {
	peers := make([]localapi.MachineStatus, len(snapshot.Machines))
	for i, m := range snapshot.Machines {
		peers[i] = localapi.MachineStatus{
			Eligible: m.Eligible, RuntimeState: m.RuntimeState, ActiveConsumers: m.ActiveConsumers, SelectedPath: m.SelectedPath,
			TransportConsumers: m.TransportConsumers, StandbyPath: m.StandbyPath, RelayRegion: m.RelayRegion,
			TransferReadiness: m.TransferReadiness, PreviewReadiness: m.PreviewReadiness, SSHReadiness: m.SSHReadiness,
			NATMappingIPv4: m.NATMappingIPv4, NATMappingIPv6: m.NATMappingIPv6, CaptivePortal: m.CaptivePortal, PMTU: m.PMTU,
			RouterProtocol: m.RouterProtocol, RouterMapping: m.RouterMapping, MappingLifetime: m.MappingLifetime,
			UpdateHealth: m.UpdateHealth, Health: m.Health,
		}
	}
	snapshot.Machines = peers
	return json.Marshal(snapshot)
}
