package serverclock

import (
	"errors"
	"net/http"
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
	if skew := Now().Sub(time.Now()); skew < 9*time.Minute {
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
	asked := make(chan struct{}, 4)
	SetRemeasure(func() { asked <- struct{}{} })
	realWall := wallNano
	t.Cleanup(func() {
		wallNano = realWall
		remeasure.Store(nil)
		offset.Store(0)
		learned.Store(nil)
		remeasured.Store(nil)
	})
	sent := time.Now()
	Observe(sent.Add(-10*time.Minute).UTC().Format(http.TimeFormat), sent, sent.Add(100*time.Millisecond)) // local clock 10 min fast
	if got := Offset(); got > -9*time.Minute-50*time.Second || got < -10*time.Minute-10*time.Second {
		t.Fatalf("offset=%v", got)
	}
	select {
	case <-asked:
		t.Fatal("asked the server again without a clock step")
	default:
	}
	wallNano = func() int64 { return realWall() - int64(10*time.Minute) } // the local clock is set back to the real time
	_ = Offset()
	select {
	case <-asked:
	case <-time.After(time.Second):
		t.Fatal("the server was not asked for its time after the clock step")
	}
	// The answer: the local clock now matches the server's, so the offset is gone.
	now := time.Now()
	Observe(now.UTC().Format(http.TimeFormat), now, now.Add(50*time.Millisecond))
	if got := Offset(); got != 0 {
		t.Fatalf("offset after the answer: %v", got)
	}
	_ = Offset()
	select {
	case <-asked:
		t.Fatal("asked again after the answer")
	default:
	}
}

// Local turns a server-clock instant into the machine-clock time a timer or net deadline needs (X-27).
func TestLocalMapsAServerInstantToTheMachineClock(t *testing.T) {
	sent := time.Now()
	serverNow := sent.Add(-10 * time.Minute) // the machine's clock is 10 minutes ahead
	Observe(serverNow.UTC().Format(http.TimeFormat), sent, sent)
	t.Cleanup(func() { Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	deadline := serverNow.Add(30 * time.Second)
	if until := time.Until(Local(deadline)); until < 29*time.Second || until > 31*time.Second {
		t.Fatalf("a server deadline 30 s away is %s away on the machine clock", until)
	}
}
