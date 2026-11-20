package serverclock

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestOffsetAppliesFromAMinute(t *testing.T) {
	t.Cleanup(func() { offset.Store(0) })
	sent := time.Now()
	Observe(sent.Add(10*time.Minute).UTC().Format(http.TimeFormat), sent, sent.Add(200*time.Millisecond))
	if got := Offset(); got < 9*time.Minute+58*time.Second || got > 10*time.Minute+2*time.Second {
		t.Fatalf("offset=%v", got)
	}
	if skew := time.Until(Now()); skew < 9*time.Minute {
		t.Fatalf("Now does not follow the server: %v", skew)
	}
	Observe(sent.Add(20*time.Second).UTC().Format(http.TimeFormat), sent, sent.Add(time.Second))
	if Offset() != 0 {
		t.Fatalf("an offset below the threshold applied: %v", Offset())
	}
	Observe("not a date", sent, sent)
	Observe(sent.Add(time.Hour).UTC().Format(http.TimeFormat), sent, sent.Add(time.Minute))
	if Offset() != 0 {
		t.Fatalf("an unusable observation changed the offset: %v", Offset())
	}
}

func TestMeasureNamesWhyItCannot(t *testing.T) {
	now := time.Now()
	if _, err := Measure("", now, now); !errors.Is(err, ErrNoDate) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := Measure("nonsense", now, now); !errors.Is(err, ErrBadDate) {
		t.Fatalf("garbage: %v", err)
	}
	if _, err := Measure(now.UTC().Format(http.TimeFormat), now, now.Add(time.Minute)); !errors.Is(err, ErrSlowRoundTrip) {
		t.Fatalf("slow: %v", err)
	}
	delta, err := Measure(now.Add(time.Hour).UTC().Format(http.TimeFormat), now, now)
	if err != nil || delta < 59*time.Minute || delta > 61*time.Minute {
		t.Fatalf("hour ahead: %v %v", delta, err)
	}
}

// X-27: a machine whose clock was 10 minutes fast and is then set back must not keep judging by the old
// offset until some unrelated response: the step makes it ask the server for its time at once.
func TestAClockSetBackMakesItAskTheServerAgain(t *testing.T) {
	var calls atomic.Int32
	asked := make(chan struct{}, 4)
	SetRemeasure(func() { calls.Add(1); asked <- struct{}{} })
	realWall, realIn := wallNano, remeasureIn
	t.Cleanup(func() {
		wallNano, remeasureIn = realWall, realIn
		remeasure.Store(nil)
		offset.Store(0)
		learned.Store(nil)
		remeasured.Store(nil)
	})
	// The ask is started inside Offset: remeasured is set before the goroutine runs, so it is a race-free verdict
	// on "was the server asked" where a channel read with no wait could not tell "no" from "not yet".
	sent := time.Now()
	Observe(sent.Add(-10*time.Minute).UTC().Format(http.TimeFormat), sent, sent.Add(100*time.Millisecond)) // local clock 10 min fast
	if got := Offset(); got > -9*time.Minute-50*time.Second || got < -10*time.Minute-10*time.Second {
		t.Fatalf("offset=%v", got)
	}
	if remeasured.Load() != nil {
		t.Fatal("asked the server again without a clock step")
	}
	wallNano = func() int64 { return realWall() - int64(10*time.Minute) } // the local clock is set back to the real time
	_ = Offset()
	if remeasured.Load() == nil {
		t.Fatal("the clock step did not start a new ask")
	}
	select {
	case <-asked:
	case <-time.After(30 * time.Second):
		t.Fatal("the server was not asked for its time after the clock step")
	}
	// The answer: the local clock now matches the server's, so the offset is gone.
	now := time.Now()
	Observe(now.UTC().Format(http.TimeFormat), now, now.Add(50*time.Millisecond))
	if got := Offset(); got != 0 {
		t.Fatalf("offset after the answer: %v", got)
	}
	// With the 10 s throttle lifted, only the absence of a step can keep Offset from asking again.
	remeasureIn = 0
	answered := remeasured.Load()
	for range 3 {
		_ = Offset()
	}
	if remeasured.Load() != answered {
		t.Fatal("asked again after the answer without a new step")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the server was asked %d times, want exactly 1", n)
	}
}
