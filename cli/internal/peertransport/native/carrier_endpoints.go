package native

import (
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// offerEndpoints is the candidate set an offer or exchange carries: at most what a peer accepts
// (tailnet.MaxPeerEndpoints). A host with many interfaces (a docker host reports 18) otherwise could not
// sign its generation at all.
func offerEndpoints(endpoints []netip.AddrPort) []string {
	return orderEndpoints(endpoints, localInterfaceNames())
}

// orderEndpoints keeps the endpoints a peer can use, best first: public addresses, then private LAN ones,
// then those on container bridges, then loopback (magicsock likewise uses loopback only when nothing else
// exists, ~/ref/tailscale/wgengine/magicsock/magicsock.go determineEndpoints; it lists STUN/public
// addresses before local ones, and leaves out IPv6 fe80:: because no case is known where they give WAN
// connectivity, ~/ref/tailscale/net/netmon/state.go LocalAddresses). The engine hands over bare addresses,
// so public-vs-STUN is told by the address itself; interface names come from the host (names).
// shortcut: no default-route preference among LAN interfaces, add it when a host with two LANs needs it.
func orderEndpoints(endpoints []netip.AddrPort, names map[netip.Addr]string) []string {
	rank := func(e netip.AddrPort) int {
		switch ip := e.Addr(); {
		case ip.IsLoopback():
			return 3
		case bridgeInterface(names[ip]):
			return 2
		case ip.IsPrivate():
			return 1
		}
		return 0
	}
	var usable []netip.AddrPort
	for _, e := range endpoints {
		if e.Addr().Zone() == "" && !e.Addr().IsLinkLocalUnicast() {
			usable = append(usable, e)
		}
	}
	slices.SortStableFunc(usable, func(a, b netip.AddrPort) int { return rank(a) - rank(b) })
	var out []string
	for _, e := range usable[:min(len(usable), tailnet.MaxPeerEndpoints)] {
		out = append(out, e.String())
	}
	return out
}

func bridgeInterface(name string) bool {
	for _, prefix := range []string{"docker", "br-", "veth", "cni", "virbr", "flannel", "cali"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func localInterfaceNames() map[netip.Addr]string {
	names := make(map[netip.Addr]string)
	interfaces, _ := net.Interfaces() // no names: ranking by address alone is still correct
	for _, iface := range interfaces {
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(ipNet.IP); ok {
					names[ip.Unmap()] = iface.Name
				}
			}
		}
	}
	return names
}
