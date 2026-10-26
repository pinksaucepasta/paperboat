package serverclock

import (
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
