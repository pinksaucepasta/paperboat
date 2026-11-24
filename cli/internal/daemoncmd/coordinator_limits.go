package daemoncmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/cli/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
)

// routableWithinLimits keeps what the machine guard serves one daemon: the first MaxRoutedMachines machines by ID
// (the same ones every time), each with its lowest ports, shared out evenly, up to MaxMachineListeners. It returns how
// many machines and ports were left out.
func routableWithinLimits(peers []*daemonrpc.PeerUpdate) (kept []*daemonrpc.PeerUpdate, machines, ports int) {
	for _, peer := range peers {
		if peer != nil && peer.GetApproved() && peer.GetOnline() && peer.GetAlias() != "" && len(peer.GetExportedPorts()) > 0 {
			kept = append(kept, peer)
		}
	}
	if len(kept) > machineguard.MaxRoutedMachines {
		slices.SortFunc(kept, func(a, b *daemonrpc.PeerUpdate) int { return strings.Compare(a.GetPeerId(), b.GetPeerId()) })
		machines = len(kept) - machineguard.MaxRoutedMachines
		kept = kept[:machineguard.MaxRoutedMachines]
	}
	total, longest := 0, 0
	for _, peer := range kept {
		total, longest = total+len(peer.GetExportedPorts()), max(longest, len(peer.GetExportedPorts()))
	}
	if total <= machineguard.MaxMachineListeners {
		return kept, machines, 0
	}
	share := func(limit int) (sum int) {
		for _, peer := range kept {
			sum += min(limit, len(peer.GetExportedPorts()))
		}
		return sum
	}
	for share(longest) > machineguard.MaxMachineListeners { // at most 65535 ports; halve, then climb back
		longest /= 2
	}
	for share(longest+1) <= machineguard.MaxMachineListeners {
		longest++
	}
	for i, peer := range kept {
		if len(peer.GetExportedPorts()) > longest {
			trimmed := *peer
			trimmed.ExportedPorts = slices.Sorted(slices.Values(peer.GetExportedPorts()))[:longest]
			kept[i] = &trimmed
		}
	}
	return kept, machines, total - share(longest)
}

// browserAliasSet collects browser names up to the machine guard's limits and counts the ones left out.
type browserAliasSet struct {
	names          map[string]string
	bytes, skipped int
}

func (s *browserAliasSet) add(name, target string) bool {
	size := len(name) + len(target)
	if _, exists := s.names[name]; exists {
		return true
	}
	if len(s.names) >= machineguard.MaxBrowserAliases || s.bytes+size > machineguard.MaxBrowserAliasBytes {
		s.skipped++
		return false
	}
	s.names[name] = target
	s.bytes += size
	return true
}

func overLimitDetail(machines, ports, browserNames int) string {
	var parts []string
	for _, part := range []struct {
		count int
		what  string
	}{{machines, "machines"}, {ports, "machine ports"}, {browserNames, "browser names"}} {
		if part.count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", part.count, part.what))
		}
	}
	return strings.Join(parts, ", ")
}
