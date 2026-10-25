package runtimeattachment

import (
	"fmt"
	"reflect"
	"testing"
)

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

func TestEndpointCarrierGroupingRoundTrips(t *testing.T) {
	base := Admission{Schema: Schema, Binding: Binding{AccountID: "a", HostID: "h", SessionID: "s1", EdgeProcessEpoch: "e", EdgeCarrierServerCertificateChainPEM: "chain"}, EdgeEndpoints: []string{"h2://edge:1"}, ConfigContentHash: "sha256:x"}
	var in []Admission
	for i, session := range []string{"s1", "s2", "s1"} {
		a := base
		a.Binding.SessionID = session
		a.Binding.RouteID, a.Binding.RouteGeneration, a.RouteRevision, a.AttachmentGeneration = fmt.Sprintf("route_%d", i), uint64(i+1), uint64(i+1), 7
		a.Hostname, a.EndpointID, a.AccessMode, a.RouteKind = fmt.Sprintf("h%d.example.test", i), fmt.Sprintf("preview_%d", i), "public", EndpointPublicRouteKind
		if i == 1 {
			a.AccessMode, a.RouteKind = "team", EndpointRestrictedRouteKind
		}
		in = append(in, a)
	}
	groups := GroupEndpointAdmissions(in)
	if len(groups) != 2 || len(groups[0].Routes) != 2 || groups[0].Carrier.Binding.RouteID != "" {
		t.Fatalf("groups = %+v", groups)
	}
	var out []Admission
	for _, g := range groups {
		out = append(out, g.Admissions()...)
	}
	byRoute := map[string]Admission{}
	for _, a := range out {
		byRoute[a.Binding.RouteID] = a
	}
	for _, a := range in {
		if !reflect.DeepEqual(byRoute[a.Binding.RouteID], a) {
			t.Fatalf("round trip changed %s:\n%+v\n%+v", a.Binding.RouteID, byRoute[a.Binding.RouteID], a)
		}
	}
}
