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

// scriptedAnswers answers each heartbeat with the next scripted drain deadline (nil: none) and records what
// the edge reported.
type scriptedAnswers struct {
	control.NodeSink
	mu        sync.Mutex
	deadlines []*int64
	seen      []control.NodeObservation
}

func (s *scriptedAnswers) Heartbeat(_ context.Context, observation control.NodeObservation) (control.NodeAnswer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, observation)
	var answer control.NodeAnswer
	if len(s.deadlines) > 0 {
		answer.DrainDeadline, s.deadlines = s.deadlines[0], s.deadlines[1:]
	}
	return answer, nil
}

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

func drainWorker(t *testing.T, sink control.NodeSink, drain *drainRecorder, pulse chan time.Time) *node.Manager {
	t.Helper()
	state := node.New("edge")
	state.MarkReady()
	manager, _ := node.NewManager(state, 2)
	worker := &NodeWorker{Manager: manager, Sink: sink, Registration: testNodeRegistration(t, "edge", "process", 2), Pulse: pulse, Now: func() time.Time { return time.Unix(10, 0) }, Drain: drain.Drain}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Shutdown(context.Background()) })
	return manager
}

func deadlineAt(unix int64) *int64 { return &unix }

// The first scripted answer goes to the worker's start; each pulse then takes the next.
// beat runs one heartbeat (the next pulse is accepted only once this one is done).
func beat(pulse chan time.Time) { pulse <- time.Unix(11, 0); pulse <- time.Unix(11, 0) }

// The first answer with drain_deadline sends the goaway to the carriers, in the background (the blocked drain does
// not stop the heartbeats), and the heartbeats that repeat the same standing order do not send it again.
func TestHeartbeatDrainDeadlineSendsGoawayOnce(t *testing.T) {
	drain := &drainRecorder{sent: make(chan struct{}, 4)}
	sink := &scriptedAnswers{NodeSink: testedge.New(), deadlines: []*int64{nil, nil, deadlineAt(1000), deadlineAt(1000), deadlineAt(1000)}}
	pulse := make(chan time.Time)
	drainWorker(t, sink, drain, pulse)
	pulse <- time.Unix(11, 0) // answer: no deadline
	if drain.count() != 0 {
		t.Fatal("drained without an order")
	}
	pulse <- time.Unix(12, 0) // answer: deadline 1000
	select {
	case <-drain.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain order sent no goaway")
	}
	pulse <- time.Unix(13, 0) // the same order again, and again
	pulse <- time.Unix(14, 0)
	pulse <- time.Unix(15, 0)
	if drain.count() != 1 || !drain.calls[0].Equal(time.Unix(1000, 0)) {
		t.Fatalf("goaway sent %d times, first for %v: want once for the deadline", drain.count(), drain.calls)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.seen) < 5 {
		t.Fatalf("the blocked drain stopped the heartbeats: %d seen", len(sink.seen))
	}
}

// A deadline already past is not a drain to announce.
func TestHeartbeatPastDrainDeadlineSendsNoGoaway(t *testing.T) {
	drain := &drainRecorder{sent: make(chan struct{}, 4)}
	sink := &scriptedAnswers{NodeSink: testedge.New(), deadlines: []*int64{nil, deadlineAt(5)}}
	pulse := make(chan time.Time)
	drainWorker(t, sink, drain, pulse)
	beat(pulse)
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
	sink := &scriptedAnswers{NodeSink: testedge.New(), deadlines: []*int64{nil, deadlineAt(1000), nil, nil, deadlineAt(2000)}}
	pulse := make(chan time.Time)
	manager := drainWorker(t, sink, drain, pulse)
	for range 4 {
		pulse <- time.Unix(11, 0)
	}
	pulse <- time.Unix(12, 0) // the fourth answer has been applied
	deadline := time.Now().Add(5 * time.Second)
	for drain.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
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
