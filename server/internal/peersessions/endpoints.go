package peersessions

import (
	"net/netip"
	"slices"
)

// MaxReportedEndpoints is how many candidate endpoints an engine may report (PLAN 3b, N5).
const MaxReportedEndpoints = 8

// normalizeEndpoints is the stored form of an engine's reported candidate endpoints: each a unicast ip:port
// (IPv4-mapped addresses unwrapped; no unspecified, loopback or multicast address, no port 0), sorted and
// deduplicated, so equal sets compare equal. nil (not reported) stays nil; an empty report is an empty set.
func normalizeEndpoints(reported []string) ([]string, error) {
	if reported == nil {
		return nil, nil
	}
	if len(reported) > MaxReportedEndpoints {
		return nil, ErrInvalid
	}
	out := make([]string, 0, len(reported))
	for _, raw := range reported {
		endpoint, err := netip.ParseAddrPort(raw)
		if err != nil {
			return nil, ErrInvalid
		}
		addr := endpoint.Addr().Unmap()
		if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.Zone() != "" || endpoint.Port() == 0 {
			return nil, ErrInvalid
		}
		out = append(out, netip.AddrPortFrom(addr, endpoint.Port()).String())
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
