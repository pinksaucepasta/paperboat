package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/node"
	"github.com/pinksaucepasta/paperboat/edge/internal/testedge"
)

// scriptedAnswers answers every heartbeat with the drain deadline last set (nil: none) and records what the edge
// reported.
type scriptedAnswers struct {
	control.NodeSink
	mu       sync.Mutex
	deadline *int64
	seen     []control.NodeObservation
}

func (s *scriptedAnswers) Heartbeat(_ context.Context, observation control.NodeObservation) (control.NodeAnswer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, observation)
	return control.NodeAnswer{DrainDeadline: s.deadline}, nil
}

func (s *scriptedAnswers) answer(deadline *int64) { s.mu.Lock(); s.deadline = deadline; s.mu.Unlock() }

type drainRecorder struct {
	mu    sync.Mutex
	calls []time.Time
	sent  chan struct{}
}

// Drain blocks like the carrier assembly's does (it waits for idle carriers or the deadline) until ctx ends.
func (d *drainRecorder) Drain(ctx context.Context, deadline time.Time) {
	d.mu.Lock()
	d.calls = append(d.calls, deadline)
	d.mu.Unlock()
	d.sent <- struct{}{}
	<-ctx.Done()
}

func (d *drainRecorder) count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.calls) }

func (d *drainRecorder) first() time.Time { d.mu.Lock(); defer d.mu.Unlock(); return d.calls[0] }

// drainWorker is driven by calling heartbeat directly: no loop, no pulses, so no timing (the loop also heartbeats once
// when the node becomes ready, which made a scripted order of answers depend on scheduling).
func drainWorker(t *testing.T, sink control.NodeSink, drain *drainRecorder) (*NodeWorker, *node.Manager, context.Context) {
	t.Helper()
	state := node.New("edge")
	state.MarkReady()
	manager, _ := node.NewManager(state, 2)
	worker := &NodeWorker{Manager: manager, Sink: sink, Registration: testNodeRegistration(t, "edge", "process", 2), Drain: drain.Drain}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); worker.drains.Wait() })
	return worker, manager, ctx
}

func deadlineAt(unix int64) *int64 { return &unix }

func beat(t *testing.T, worker *NodeWorker, ctx context.Context, unix int64) {
	t.Helper()
	if err := worker.heartbeat(ctx, time.Unix(unix, 0)); err != nil {
		t.Fatal(err)
	}
}

func waitSent(t *testing.T, drain *drainRecorder, what string) {
	t.Helper()
	select {
	case <-drain.sent:
	case <-time.After(5 * time.Second):
		t.Fatal(what)
	}
}

// The first answer with drain_deadline sends the goaway to the carriers, in the background (the blocked drain does
// not stop the heartbeats), and the heartbeats that repeat the same standing order do not send it again.
func TestHeartbeatDrainDeadlineSendsGoawayOnce(t *testing.T) {
	drain := &drainRecorder{sent: make(chan struct{}, 4)}
	sink := &scriptedAnswers{NodeSink: testedge.New()}
	worker, _, ctx := drainWorker(t, sink, drain)
	beat(t, worker, ctx, 11) // answer: no deadline
	if drain.count() != 0 {
		t.Fatal("drained without an order")
	}
	sink.answer(deadlineAt(1000))
	beat(t, worker, ctx, 12)
	waitSent(t, drain, "the drain order sent no goaway")
	for at := int64(13); at <= 15; at++ { // the same order again, and again, while the first drain still blocks
		beat(t, worker, ctx, at)
	}
	if drain.count() != 1 || !drain.first().Equal(time.Unix(1000, 0)) {
		t.Fatalf("goaway sent %d times, first for %v: want once for the deadline", drain.count(), drain.first())
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.seen) != 5 {
		t.Fatalf("%d heartbeats seen, want 5: the blocked drain must not stop them", len(sink.seen))
	}
}

// A deadline already past is not a drain to announce.
func TestHeartbeatPastDrainDeadlineSendsNoGoaway(t *testing.T) {
	drain := &drainRecorder{sent: make(chan struct{}, 4)}
	sink := &scriptedAnswers{NodeSink: testedge.New()}
	worker, _, ctx := drainWorker(t, sink, drain)
	sink.answer(deadlineAt(5))
	beat(t, worker, ctx, 10)
	if drain.count() != 0 {
		t.Fatal("a goaway was sent for a deadline in the past")
	}
}

// After a Resume (the answer has no deadline) the edge is the same ready process: its state never left ready,
// every heartbeat reported it ready and not draining, and a later drain order sends a new goaway. Carriers
// keep being accepted throughout (the drain only tells hosts to move; datacarrier.Service is never closed), and
// TestCarrierStaysOpenWhenItsAdmissionLeavesThePullAndServesAgainWhenItReturns pins the carrier serving again.
func TestEdgeServesAgainAfterResume(t *testing.T) {
	drain := &drainRecorder{sent: make(chan struct{}, 4)}
	sink := &scriptedAnswers{NodeSink: testedge.New()}
	worker, manager, ctx := drainWorker(t, sink, drain)
	sink.answer(deadlineAt(1000))
	beat(t, worker, ctx, 11)
	waitSent(t, drain, "the first drain order sent no goaway")
	sink.answer(nil) // Resume
	beat(t, worker, ctx, 12)
	beat(t, worker, ctx, 13)
	sink.answer(deadlineAt(2000))
	beat(t, worker, ctx, 14)
	waitSent(t, drain, "the drain order after the Resume sent no goaway")
	if drain.count() != 2 {
		t.Fatalf("goaways = %d, want one per drain order (before and after the Resume)", drain.count())
	}
	if snapshot := manager.Snapshot(); snapshot.Draining {
		t.Fatalf("node after the orders = %+v: a drain order must not end this process's readiness", snapshot)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, observation := range sink.seen {
		if !observation.Ready || observation.Draining {
			t.Fatalf("heartbeat reported %+v, want ready and not draining throughout", observation)
		}
	}
}
