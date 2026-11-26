package edgeports

import (
	"strings"
	"testing"
)

func TestRangesAreValidAndDisjoint(t *testing.T) {
	if err := Check(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicTCPBounds(t *testing.T) {
	for port, want := range map[int]bool{PublicTCPMin - 1: false, PublicTCPMin: true, 24443: true, PublicTCPMax: true, PublicTCPMax + 1: false, 443: false} {
		if PublicTCP(port) != want {
			t.Errorf("PublicTCP(%d) = %v, want %v", port, !want, want)
		}
	}
}

func TestNodePortsAreDistinctAndClearOfThePublicRange(t *testing.T) {
	seen := map[int]bool{}
	for _, p := range []int{CarrierTCP, CarrierQUIC, RelayWSS, RelayQUIC, RelayMesh, Claim, EdgeHealth, EdgePrivateHTTPS} {
		if p < 1024 || p >= PublicTCPMin || seen[p] {
			t.Fatalf("node port %d is privileged, inside the public range or repeated", p)
		}
		seen[p] = true
	}
}

func TestInboundRulesPerRole(t *testing.T) {
	text := func(rules []Rule) string {
		var s []string
		for _, r := range rules {
			s = append(s, r.Protocol+" "+r.Ports)
		}
		return strings.Join(s, ", ")
	}
	for role, want := range map[string]string{
		"relay":  "tcp 7277, tcp 443, udp 443, udp 7275",
		"tunnel": "tcp 7277, tcp 443, udp 443, tcp 7272, udp 7273, tcp 10000-59999",
		"both":   "tcp 7277, tcp 7274, udp 7274, udp 7275, tcp 443, udp 443, tcp 7272, udp 7273, tcp 10000-59999",
	} {
		if got := text(InboundRules(role, Claim)); got != want {
			t.Errorf("%s: %s, want %s", role, got, want)
		}
	}
}
