package connectorprotocol

import (
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// A machine whose clock is 10 minutes fast (X-27) must judge connector grants on the server's clock.
func TestRealClockFollowsTheServerClock(t *testing.T) {
	sent := time.Now()
	serverclock.Observe(sent.Add(-10*time.Minute).UTC().Format(http.TimeFormat), sent, sent.Add(100*time.Millisecond))
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	if skew := time.Since(RealClock{}.Now()); skew < 9*time.Minute {
		t.Fatalf("RealClock ignores the learned server offset: local - clock = %v", skew)
	}
}
