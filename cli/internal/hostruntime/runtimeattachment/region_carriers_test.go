package runtimeattachment

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// listNodes makes the answer list exactly these edge nodes: the first is the primary carrier and the
// active edge, the rest follow in `more` (the server lists every ready node of the pair's two regions).
func listNodes(a *admission, ids ...string) {
	epoch := func(id string) string { return fmt.Sprintf("epoch-%s-%s", id, strings.Repeat("0", 30)) }
	a.Backup, a.More = nil, nil
	a.Binding.EdgeNodeID, a.Binding.EdgeProcessEpoch = ids[0], epoch(ids[0])
	a.ActiveEdgeNodeID, a.ActiveEdgeProcessEpoch = ids[0], epoch(ids[0])
	for _, id := range ids[1:] {
		next := a.primary()
		next.Binding.EdgeNodeID, next.Binding.EdgeProcessEpoch = id, epoch(id)
		a.More = append(a.More, next)
	}
}

func nodeIDs(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%d", prefix, i+1)
	}
	return ids
}

func heldNodes(service *Service) map[string]bool {
	hubs, _ := service.Hubs()
	held := map[string]bool{}
	for _, hub := range hubs {
		node, _ := hub.Edge()
		held[node] = true
	}
	return held
}

func waitHeld(t *testing.T, service *Service, within time.Duration, ok func(map[string]bool) bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if ok(heldNodes(service)) {
			return
		}
	}
	t.Fatalf("%s: holding %v", what, heldNodes(service))
}

// Every ready node of the pair's two regions is listed; the machine holds one carrier to each, up to the cap.
func TestHoldsEveryCarrierOfTheAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ids := nodeIDs("n", runtimewire.MaximumCarriers)
	h := startRuntimeCarrier(t, ctx, runtimeCarrierOptions{
		expiry: time.Now().Add(time.Hour), status: func() int { return http.StatusOK }, confirmActive: true,
		tweak: func(a *admission) { listNodes(a, ids...) },
	})
	waitHeld(t, h.service, 10*time.Second, func(held map[string]bool) bool { return len(held) == len(ids) }, "not every listed node got a carrier")
	time.Sleep(2500 * time.Millisecond) // two rounds: nothing is closed and nothing is dialed twice
	held := heldNodes(h.service)
	for _, id := range ids {
		if !held[id] {
			t.Fatalf("carrier to %s was closed: holding %v", id, held)
		}
	}
	if len(h.edges) != len(ids) {
		t.Fatalf("%d carrier dials for %d nodes", len(h.edges), len(ids))
	}
}

// A node leaves the answer and another joins (a drained node replaced in its region): the carrier to
// the node the server stopped listing goes once the answer is full again, the others stay.
func TestDropsOneCarrierWhenTheServerStopsListingIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	wake := make(chan struct{}, 1)
	ids := nodeIDs("n", runtimewire.MaximumCarriers)
	h := startRuntimeCarrier(t, ctx, runtimeCarrierOptions{
		expiry: time.Now().Add(time.Hour), status: func() int { return http.StatusOK }, confirmActive: true, wake: wake,
		tweak: func(a *admission) { listNodes(a, ids...) },
	})
	waitHeld(t, h.service, 10*time.Second, func(held map[string]bool) bool { return len(held) == len(ids) }, "not every listed node got a carrier")
	last := ids[len(ids)-1]
	h.update(func(a *admission) { listNodes(a, append(ids[:len(ids)-1:len(ids)-1], "replacement")...) })
	wake <- struct{}{}
	waitHeld(t, h.service, 10*time.Second, func(held map[string]bool) bool { return held["replacement"] && !held[last] }, "the replacement did not take over from the unlisted node")
	held := heldNodes(h.service)
	if len(held) != len(ids) {
		t.Fatalf("holding %d carriers after the swap, want %d: %v", len(held), len(ids), held)
	}
	for _, id := range ids[:len(ids)-1] {
		if !held[id] {
			t.Fatalf("carrier to %s, still listed, was closed: %v", id, held)
		}
	}
}
