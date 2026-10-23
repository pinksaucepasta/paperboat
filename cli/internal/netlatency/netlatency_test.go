package netlatency

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeReportsTheFastestSuccessPerNodeAndSkipsSilentOnes(t *testing.T) {
	var calls atomic.Int32
	dial := func(ctx context.Context, address string) error {
		switch address {
		case "fast:1":
			time.Sleep(time.Duration(5+calls.Add(1)%3) * time.Millisecond)
			return nil
		case "slow:1":
			time.Sleep(30 * time.Millisecond)
			return nil
		case "down:1":
			return errors.New("refused")
		default: // silent: only the attempt deadline ends it
			<-ctx.Done()
			return ctx.Err()
		}
	}
	targets := []Target{{"node_slow", "slow:1"}, {"node_down", "down:1"}, {"node_fast", "fast:1"}}
	started := time.Now()
	got := Probe(t.Context(), targets, dial)
	if len(got) != 2 || got[0].NodeID != "node_fast" || got[1].NodeID != "node_slow" || got[0].RTTMillis < 1 || got[1].RTTMillis < 30 {
		t.Fatalf("measurements=%+v", got)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("probe was not concurrent: %v", time.Since(started))
	}
}

func TestProbeBoundsTargetsAndStopsWhenCanceled(t *testing.T) {
	var targets []Target
	for i := range MaxTargets + 20 {
		targets = append(targets, Target{NodeID: string(rune('a'+i%26)) + string(rune('A'+i/26)), Address: "x:1"})
	}
	var seen atomic.Int32
	got := Probe(t.Context(), targets, func(context.Context, string) error { seen.Add(1); return nil })
	if len(got) != MaxTargets || int(seen.Load()) != MaxTargets*Attempts {
		t.Fatalf("measured %d nodes with %d dials", len(got), seen.Load())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := Probe(ctx, targets[:3], func(context.Context, string) error { return nil }); len(got) != 0 {
		t.Fatalf("canceled probe still reported %v", got)
	}
}

func TestReporterReportsOnScheduleAndSurvivesFailures(t *testing.T) {
	var rounds, reported atomic.Int32
	failures := make(chan string, 8)
	reports := make(chan []Measurement, 8)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reporter := &Reporter{
		Targets: func(context.Context) ([]Target, error) {
			if rounds.Add(1) == 1 {
				return nil, errors.New("inventory unavailable")
			}
			return []Target{{"node_a", "a:1"}}, nil
		},
		Report: func(_ context.Context, m []Measurement) error {
			if reported.Add(1) == 1 {
				return errors.New("server refused")
			}
			reports <- m
			return nil
		},
		OnFailure: func(stage string, _ error) { failures <- stage },
		Dial:      func(context.Context, string) error { return nil },
		First:     5 * time.Millisecond, Every: 10 * time.Millisecond,
	}
	done := make(chan struct{})
	go func() { reporter.Run(ctx); close(done) }()
	select {
	case m := <-reports:
		if len(m) != 1 || m[0].NodeID != "node_a" {
			t.Fatalf("report=%+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no report after earlier failures")
	}
	if first, second := <-failures, <-failures; first != "targets" || second != "report" {
		t.Fatalf("failures were %q then %q", first, second)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reporter ignored cancellation")
	}
}

func TestReporterBacksOffWhileTheServerRefusesAndRecovers(t *testing.T) {
	var times []time.Time
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reporter := &Reporter{
		Targets: func(context.Context) ([]Target, error) { return []Target{{"node_a", "a:1"}}, nil },
		Report: func(context.Context, []Measurement) error {
			mu.Lock()
			defer mu.Unlock()
			times = append(times, time.Now())
			if len(times) < 4 {
				return errors.New("not found")
			}
			cancel()
			return nil
		},
		Dial:  func(context.Context, string) error { return nil },
		First: time.Millisecond, Every: 10 * time.Millisecond,
	}
	reporter.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 4 {
		t.Fatalf("rounds=%d", len(times))
	}
	first, last := times[1].Sub(times[0]), times[3].Sub(times[2])
	if last < first+first/2 {
		t.Fatalf("waits did not grow while failing: %v then %v", first, last)
	}
}

// One connect per node and round; the report keeps the node's fastest of its last rounds, so a single
// slow handshake does not show, and a node that stops answering is dropped.
func TestReporterSmoothsOverTheLastRounds(t *testing.T) {
	var r Reporter
	round := func(ms map[string]int64) map[string]int64 {
		var in []Measurement
		for node, rtt := range ms {
			in = append(in, Measurement{NodeID: node, RTTMillis: rtt})
		}
		out := map[string]int64{}
		for _, m := range r.smooth(in) {
			out[m.NodeID] = m.RTTMillis
		}
		return out
	}
	round(map[string]int64{"a": 20, "b": 40})
	if got := round(map[string]int64{"a": 90, "b": 35}); got["a"] != 20 || got["b"] != 35 {
		t.Fatalf("second round = %v, want a=20 (fastest so far) b=35", got)
	}
	round(map[string]int64{"a": 80})
	if got := round(map[string]int64{"a": 70, "b": 50}); got["a"] != 70 || got["b"] != 50 {
		t.Fatalf("after three newer rounds = %v, want a=70 (20 left the window) and b restarted at 50", got)
	}
}
