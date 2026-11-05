package native

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// A docker host reports 18 local endpoints; a peer accepts MaxPeerEndpoints, and the generation could not be
// signed with more (every delegated-carrier test failed "invalid native session" on anvit). What stays must
// be the endpoints a peer can reach.
func TestOfferEndpointsStayWithinWhatAPeerAccepts(t *testing.T) {
	names := map[netip.Addr]string{}
	var endpoints []netip.AddrPort
	add := func(addr, iface string) {
		ap := netip.MustParseAddrPort(addr)
		endpoints = append(endpoints, ap)
		names[ap.Addr()] = iface
	}
	add("[fe80::1%eth0]:64578", "eth0")
	add("169.254.3.4:64578", "eth0")
	for i := range 10 { // sorts before the LAN address, as the engine's order does
		add(fmt.Sprintf("172.%d.0.1:64578", 17+i), fmt.Sprintf("br-%d", i))
	}
	add("172.17.0.1:64578", "docker0")
	add("192.168.1.20:64578", "eth0")
	add("10.0.0.76:64578", "wlan0")
	add("203.0.113.7:64578", "")
	add("[2001:db8::7]:64578", "eth0")
	add("127.0.0.1:64578", "lo")
	if len(endpoints) < 18 {
		t.Fatal("fixture too small", len(endpoints))
	}
	slices.SortFunc(endpoints, func(a, b netip.AddrPort) int { return a.Compare(b) }) // the engine reports them sorted
	got := orderEndpoints(endpoints, names)
	if len(got) != tailnet.MaxPeerEndpoints {
		t.Fatal("kept", len(got), got)
	}
	want := []string{"203.0.113.7:64578", "[2001:db8::7]:64578", "10.0.0.76:64578", "192.168.1.20:64578"}
	if !slices.Equal(got[:4], want) {
		t.Fatal("public then LAN first, got", got)
	}
	for _, e := range got {
		if ap := netip.MustParseAddrPort(e); ap.Addr().IsLinkLocalUnicast() || ap.Addr().IsLoopback() {
			t.Fatal("kept an unusable endpoint", e, got)
		}
	}
	if got := offerEndpoints([]netip.AddrPort{endpoints[0], netip.MustParseAddrPort("169.254.3.4:1"), netip.MustParseAddrPort("[fe80::1%eth0]:1")}); len(got) != 1 {
		t.Fatal("a short list kept link-local entries, got", got)
	}
}
