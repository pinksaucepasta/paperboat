package edgehttp

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// A node announces every hostname whose carrier is attached here: previews and runtime hostnames
// (`<machine>.<runtime base>`, kind runtime_https_wss) live in the same registry, and a detached or closed
// carrier is withdrawn.
func TestHeldAnnouncesPreviewAndRuntimeHostnamesWithAttachedCarriers(t *testing.T) {
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	runtimeIdentity := testEdgePreviewIdentity(1, 1)
	runtimeServer, runtimeClient := testEdgePreviewCarrierPair(t, runtimeIdentity)
	defer runtimeClient.Close()
	defer runtimeServer.Close()
	previewServer, previewClient := testEdgePreviewCarrierPair(t, testEdgePreviewIdentity(2, 2))
	defer previewClient.Close()
	defer previewServer.Close()
	runtimeRoute := DataCarrierPreviewRoute{RouteID: "runtime_browser", Hostname: "machine.runtime.example.test", Kind: runtimewire.RouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Identity: runtimeIdentity, Server: runtimeServer, ExpiresAt: time.Now().Add(time.Minute)}
	previewRoute := DataCarrierPreviewRoute{RouteID: "preview_01", Hostname: "app.runtime.example.test", Kind: dataCarrierPreviewRouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, Server: previewServer}
	for _, r := range []DataCarrierPreviewRoute{runtimeRoute, previewRoute} {
		if err := registry.Attach(r); err != nil {
			t.Fatal(err)
		}
	}
	matcher := NewPreviewCarrierRouteMatcher(registry)
	composite := NewCompositeRouteMatcher(matcher)
	want := []string{"app.runtime.example.test", "machine.runtime.example.test"}
	for name, held := range map[string][]string{"matcher": matcher.Held(), "composite": composite.Held(), "registry": registry.HeldHosts()} {
		slices.Sort(held)
		if !slices.Equal(held, want) {
			t.Fatalf("%s held %v, want %v", name, held, want)
		}
	}
	if err := runtimeServer.Close(); err != nil {
		t.Fatal(err)
	}
	<-runtimeServer.Done()
	deadline := time.Now().Add(2 * time.Second)
	for !slices.Equal(matcher.Held(), []string{"app.runtime.example.test"}) {
		if time.Now().After(deadline) {
			t.Fatalf("a closed carrier is still announced: %v", matcher.Held())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if (*PreviewCarrierRouteMatcher)(nil).Held() != nil || (*CompositeRouteMatcher)(nil).Held() != nil || (*DataCarrierRouteRegistry)(nil).HeldHosts(nil) != nil {
		t.Fatal("nil owners must hold nothing")
	}
}

// A one-label wildcard route with an attached carrier is announced as its *.suffix key. A rule the route layer refuses
// (a star in a suffix, an empty suffix, a star or question mark in a hostname) is not applied, so nothing is announced.
func TestHeldEmitsValidatedWildcardOnly(t *testing.T) {
	cases := []struct {
		name    string
		match   string
		host    string
		suffix  string
		refused bool
		want    []string
	}{
		{name: "one-label wildcard", match: route.MatchOneLabelWildcard, suffix: "Example.test", want: []string{"*.example.test"}},
		{name: "multi-label star suffix", match: route.MatchOneLabelWildcard, suffix: "*.example.test", refused: true},
		{name: "empty suffix", match: route.MatchOneLabelWildcard, refused: true},
		{name: "star inside an exact hostname", match: route.MatchExact, host: "a.*.example.test", refused: true},
		{name: "question mark in an exact hostname", match: route.MatchExact, host: "a?.example.test", refused: true},
		{name: "exact hostname", match: route.MatchExact, host: "App.Example.test", want: []string{"app.example.test"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := replicaRule()
			rule.Generation = 1
			rule.PathPrefix = "/"
			rule.Target = rule.RouteID
			rule.MatchType = tc.match
			rule.Hostname = tc.host
			rule.WildcardSuffix = tc.suffix
			got, err := heldForRule(t, rule)
			if tc.refused != (err != nil) || err != nil && !errors.Is(err, route.ErrInvalid) {
				t.Fatalf("apply error %v, want refused %v with route.ErrInvalid", err, tc.refused)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("held %v, want %v", got, tc.want)
			}
		})
	}
}

// heldForRule attaches a ready carrier for rule, applies rule as the route generation, and returns the keys the registry
// announces for it, with the route layer's error for the rule (nothing is announced when the rule is refused).
func heldForRule(t *testing.T, rule route.RouteRule) ([]string, error) {
	t.Helper()
	carrier, connector := testEdgePreviewCarrierPair(t, replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3))
	t.Cleanup(func() {
		connector.Close()
		carrier.Close()
	})
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.AttachReplica(carrier, "key", "thumb", replicaState(rule, 3, rule.AssignmentID, "region_01", time.Millisecond, 2)); err != nil {
		t.Fatal(err)
	}
	routes := route.NewRegistry("preview.example.test", "runtime.example.test")
	applyErr := routes.ApplyGeneration(context.Background(), 1, []route.RouteRule{rule}, func(context.Context, []route.RouteRule) error { return nil }, 0)
	return registry.HeldHosts(routes), applyErr
}
