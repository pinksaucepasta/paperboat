package route

import (
	"strings"
	"testing"
)

func TestManagedTunnelHostnameUsesDNSOwnershipRatherThanIdentityFormat(t *testing.T) {
	for _, label := range []string{"smart-cloudy-voyage-6024", "123e4567-e89b-12d3-a456-426614174000", "retained-name"} {
		host := label + ".tunnels.example.test"
		if !ValidManagedTunnelHostname(host, "tunnels.example.test") {
			t.Errorf("server-owned hostname rejected: %s", host)
		}
		registry := NewRegistry("preview.example.test", "runtime.example.test")
		rule := RouteRule{ID: "managed_route", Revision: 1, Kind: TunnelHTTPSWSS, MatchType: MatchManagedExact, Hostname: host, PathPrefix: "/", Target: "carrier_01", Protocol: "http", OriginScheme: "http", AccessMode: "public", Generation: 1}
		if err := registry.StageGeneration(1, []RouteRule{rule}); err != nil {
			t.Errorf("managed hostname stage: %v", err)
		}
	}
	for _, host := range []string{
		"nested.smart-cloudy-voyage-6024.tunnels.example.test",
		"smart-cloudy-voyage-6024.other.example.test",
		"Smart-cloudy-voyage-6024.tunnels.example.test",
		"-smart.tunnels.example.test", "smart-.tunnels.example.test",
		"smart_cloudy.tunnels.example.test", "smart..tunnels.example.test",
		"smart.tunnels.example.test:443", "smart.tunnels.example.test.",
		strings.Repeat("a", 64) + ".tunnels.example.test",
	} {
		if ValidManagedTunnelHostname(host, "tunnels.example.test") {
			t.Errorf("invalid or unowned hostname accepted: %s", host)
		}
	}
}
