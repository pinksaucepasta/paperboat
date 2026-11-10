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
