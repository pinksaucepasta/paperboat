package edgehttp

import (
	"slices"
	"testing"
	"time"

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
