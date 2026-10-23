package runtimeattachment

import "testing"

func TestValidCapabilitiesAcceptsEndpointsOptionallyWithTunnels(t *testing.T) {
	for _, tc := range []struct {
		values []string
		valid  bool
	}{
		{nil, true},
		{[]string{CapabilityEndpoints}, true},
		{[]string{CapabilityEndpoints, CapabilityTunnelEndpoints}, true},
		{[]string{CapabilityTunnelEndpoints, CapabilityEndpoints}, true},
		{[]string{CapabilityTunnelEndpoints}, false},
		{[]string{CapabilityEndpoints, CapabilityEndpoints}, false},
		{[]string{CapabilityEndpoints, "other"}, false},
		{[]string{CapabilityEndpoints, CapabilityTunnelEndpoints, "other"}, false},
	} {
		if got := ValidCapabilities(tc.values); got != tc.valid {
			t.Fatalf("ValidCapabilities(%v) = %v, want %v", tc.values, got, tc.valid)
		}
	}
	if !HasCapability([]string{CapabilityEndpoints, CapabilityTunnelEndpoints}, CapabilityTunnelEndpoints) || HasCapability([]string{CapabilityEndpoints}, CapabilityTunnelEndpoints) {
		t.Fatal("HasCapability")
	}
}

func TestValidEdgeCapabilitiesAddEndpointDeltas(t *testing.T) {
	for _, tc := range []struct {
		values []string
		valid  bool
	}{
		{nil, true},
		{[]string{CapabilityEndpoints, CapabilityTunnelEndpoints, CapabilityEndpointDeltas}, true},
		{[]string{CapabilityEndpointDeltas, CapabilityEndpoints}, true},
		{[]string{CapabilityEndpointDeltas}, false},
		{[]string{CapabilityEndpoints, CapabilityEndpointDeltas, CapabilityEndpointDeltas}, false},
		{[]string{CapabilityEndpoints, "other"}, false},
	} {
		if got := ValidEdgeCapabilities(tc.values); got != tc.valid {
			t.Fatalf("ValidEdgeCapabilities(%v) = %v, want %v", tc.values, got, tc.valid)
		}
	}
	if ValidCapabilities([]string{CapabilityEndpoints, CapabilityEndpointDeltas}) {
		t.Fatal("an agent may not advertise endpoint deltas")
	}
}
