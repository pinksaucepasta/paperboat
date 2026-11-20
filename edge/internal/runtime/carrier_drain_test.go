package runtime

import (
	"context"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
)

// A node past its drain deadline stops listing its admissions. The edge then detaches the routes at once
// but leaves the carrier open (the host ends it, or the process leaving does), and a node that is resumed
// lists the admission again: the same carrier serves it with no reconnect. The region plan relies on both:
// a machine keeps one carrier to every node of its two regions, whether or not the node is listed.
func TestCarrierStaysOpenWhenItsAdmissionLeavesThePullAndServesAgainWhenItReturns(t *testing.T) {
	a := runtimeAdmission(t)
	source := &runtimeSourceFake{admissions: []runtimewire.Admission{a}}
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: a.Binding.EdgeNodeID, ProcessEpoch: a.Binding.EdgeProcessEpoch})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: a.Binding.EdgeProcessEpoch})
	if err != nil {
		t.Fatal(err)
	}
	w := &RuntimeCarrierWorker{Source: source, Expected: expected, Routes: routes, Endpoints: endpointRegistry(t, a.Binding.EdgeProcessEpoch), Interval: 20 * time.Millisecond, Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Shutdown(context.Background()) }()
	waitRuntime(t, func() bool { return len(expected.Snapshot()) == 1 })
	ea, err := control.ExpectedRuntimeAdmission(a, a.Binding.EdgeNodeID, a.Binding.EdgeProcessEpoch, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := newTestCarrierServer(t, ea.Identity)
	done := make(chan error, 1)
	go func() { done <- w.Handle(ctx, server) }()
	waitRuntime(t, func() bool { return routes.Len() == 1 })

	source.mu.Lock()
	source.admissions = nil // the node is past its drain deadline
	source.mu.Unlock()
	waitRuntime(t, func() bool { return routes.Len() == 0 })
	source.pullsAfter(t, 3)
	select {
	case <-server.Done():
		t.Fatal("the edge closed the carrier when its admission left the pull")
	case err := <-done:
		t.Fatalf("the carrier handler ended when its admission left the pull: %v", err)
	default:
	}

	source.mu.Lock()
	source.admissions = []runtimewire.Admission{a} // resumed
	source.mu.Unlock()
	waitRuntime(t, func() bool { return routes.Len() == 1 })
	_ = server.Close()
	select {
	case <-done:
	case <-time.After(runtimeWaitBound):
		t.Fatal("handler leaked")
	}
}
