package machineevents_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
)

func waitReturns(sub *machineevents.Subscription, base time.Duration) <-chan time.Duration {
	result := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		sub.Wait(context.Background(), base)
		result <- time.Since(started)
	}()
	return result
}

// A published change wakes the subscriber well inside a second although its own poll is an hour away.
func TestChangeWakesSubscriberWithoutPolling(t *testing.T) {
	_, subs, plane := eventstest.Start(t, time.Hour, machineevents.TopicPreviews, machineevents.TopicMachineAccess)
	waitDone := waitReturns(subs[0], time.Hour)
	time.Sleep(50 * time.Millisecond)
	select {
	case elapsed := <-waitDone:
		t.Fatalf("woke after %v without a change", elapsed)
	default:
	}
	plane.Publish(machineevents.TopicPreviews)
	select {
	case elapsed := <-waitDone:
		if elapsed > time.Second {
			t.Fatalf("woke after %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber was not woken by the change")
	}
	select {
	case <-subs[1].Changed():
		t.Fatal("a change to one topic woke the other topic's subscriber")
	default:
	}
}

// The request is signed like the other host calls and carries the generations already seen.
func TestRequestIsSignedAndCarriesKnownGenerations(t *testing.T) {
	_, subs, plane := eventstest.Start(t, time.Hour, machineevents.TopicPreviews)
	waitDone := waitReturns(subs[0], time.Hour)
	plane.Publish(machineevents.TopicPreviews)
	<-waitDone
	deadline := time.After(2 * time.Second)
	for len(plane.Requests()) < 2 {
		select {
		case <-deadline:
			t.Fatalf("requests=%v", plane.Requests())
		case <-time.After(10 * time.Millisecond):
		}
	}
	requests := plane.Requests()
	if requests[0]["previews"] != 0 || requests[1]["previews"] != 1 {
		t.Fatalf("generations sent = %v", requests)
	}
	header := plane.Headers(0)
	proof := plane.NextProof()
	if header.Get("X-Paperboat-Machine-Identity") != "identity-token" || header.Get("Idempotency-Key") != proof.OperationID || proof.Method != http.MethodPost || proof.Path != machineevents.Path {
		t.Fatalf("headers=%v proof=%+v", header, proof)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(header.Get("X-Paperboat-Machine-Proof")); err != nil || string(raw) != "proof-bytes" {
		t.Fatalf("proof header = %q (%v)", header.Get("X-Paperboat-Machine-Proof"), err)
	}
	if first, second := header.Get("Idempotency-Key"), plane.Headers(1).Get("Idempotency-Key"); first == second {
		t.Fatalf("operation id reused: %s", first)
	}
}

// While the loop is healthy the subscriber's own poll is the slow fallback; a subscriber without a
// hub keeps its own interval.
func TestPollingSlowsOnlyWhileTheLoopIsHealthy(t *testing.T) {
	hub, subs, _ := eventstest.Start(t, 600*time.Millisecond, machineevents.TopicPreviews)
	deadline := time.Now().Add(3 * time.Second)
	for !hub.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("loop never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if elapsed := <-waitReturns(subs[0], 20*time.Millisecond); elapsed < 500*time.Millisecond {
		t.Fatalf("healthy loop still polled after %v", elapsed)
	}
	var none *machineevents.Subscription
	started := time.Now()
	none.Wait(t.Context(), 20*time.Millisecond)
	if time.Since(started) > 300*time.Millisecond {
		t.Fatalf("nil subscription waited %v", time.Since(started))
	}
}

// After failures the loop reports unhealthy, so polls are normal again; it reconnects with what it
// knew and is told about everything that moved while it was away.
func TestFailureMakesPollsNormalAndReconnectCatchesUp(t *testing.T) {
	hub, subs, plane := eventstest.Start(t, time.Hour, machineevents.TopicPreviews)
	waitDone := waitReturns(subs[0], time.Hour)
	plane.Publish(machineevents.TopicPreviews)
	<-waitDone
	plane.FailNext(1)
	// The request already held open finishes first; the failure hits the one after it.
	deadline := time.Now().Add(5 * time.Second)
	for hub.Healthy() || plane.PendingFailures() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("loop did not report the failure")
		}
		time.Sleep(5 * time.Millisecond)
	}
	plane.Publish(machineevents.TopicPreviews) // moved while the loop is backing off
	if elapsed := <-waitReturns(subs[0], 30*time.Millisecond); elapsed > time.Second {
		t.Fatalf("unhealthy loop delayed the subscriber's poll by %v", elapsed)
	}
	select {
	case elapsed := <-waitReturns(subs[0], time.Hour):
		if elapsed > 4*time.Second {
			t.Fatalf("catch-up took %v", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the change that moved during the outage was never delivered")
	}
}

type tokenAndProof struct{}

func (tokenAndProof) Token(context.Context) (string, error) { return "t", nil }
func (tokenAndProof) Proof(context.Context, string, string, string, []byte) ([]byte, error) {
	return []byte("p"), nil
}

func TestNewRejectsAnInsecureOrIncompleteConfiguration(t *testing.T) {
	base := machineevents.Config{ControlURL: "https://control.example.test", Tokens: tokenAndProof{}, Proofs: tokenAndProof{}, OperationID: func() (string, error) { return "x", nil }, Transport: http.DefaultTransport}
	if _, err := machineevents.New(base); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*machineevents.Config){
		"http":      func(c *machineevents.Config) { c.ControlURL = "http://control.example.test" },
		"no tokens": func(c *machineevents.Config) { c.Tokens = nil },
		"no proofs": func(c *machineevents.Config) { c.Proofs = nil },
		"no op id":  func(c *machineevents.Config) { c.OperationID = nil },
		"transport": func(c *machineevents.Config) { c.Transport = nil },
	} {
		config := base
		mutate(&config)
		if _, err := machineevents.New(config); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// A daemon stopped right after it started (pb update restarts it twice within seconds) shuts its hub down
// before the loop goroutine has run. The loop must still close the channel Shutdown waits on, not panic
// on a field Shutdown has already cleared.
func TestShutdownRightAfterStartStopsTheLoopWithoutPanicking(t *testing.T) {
	wait := func(ctx context.Context, _ map[machineevents.Topic]int64) (map[machineevents.Topic]int64, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	for range 1000 {
		hub, err := machineevents.NewWithWaiter(wait, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := hub.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err = hub.Shutdown(ctx)
		cancel()
		if err != nil {
			t.Fatalf("shutdown right after start: %v", err)
		}
	}
}

// A newly paired machine enters the peer mesh when it first comes online, which moves only inventory;
// a host engine waiting on peer_network alone learned of it at its next periodic refresh (N2 on pbvm2:
// the first exec from a fresh machine waited 12 s for the relay to forward to the existing one). One
// subscription to several topics wakes on any of them.
func TestSubscriptionToSeveralTopicsWakesOnEach(t *testing.T) {
	hub, _, plane := eventstest.Start(t, time.Hour, machineevents.TopicPreviews)
	sub := hub.Subscribe(machineevents.TopicPeerNetwork, machineevents.TopicInventory)
	drain := func() {
		select {
		case <-sub.Changed():
		case <-time.After(200 * time.Millisecond):
		}
	}
	drain() // a subscription to a newly known topic may wake once
	for _, topic := range []machineevents.Topic{machineevents.TopicInventory, machineevents.TopicPeerNetwork} {
		plane.Publish(topic)
		select {
		case <-sub.Changed():
		case <-time.After(2 * time.Second):
			t.Fatalf("a change to %s did not wake the subscription", topic)
		}
	}
	plane.Publish(machineevents.TopicPreviews)
	select {
	case <-sub.Changed():
		t.Fatal("a change to another topic woke the subscription")
	case <-time.After(300 * time.Millisecond):
	}
}
