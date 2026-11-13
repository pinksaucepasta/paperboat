package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// TestRelativeTimeIsOnTheServerClock: a machine whose clock is 10 minutes ahead learns the server's time and must
// show a session created 5 minutes ago on the server as 5m, not 15m (X-27); a future expiry reads "in", not "now".
func TestRelativeTimeIsOnTheServerClock(t *testing.T) {
	sent := time.Now()
	serverNow := sent.Add(-10 * time.Minute) // the server is 10 minutes behind this machine
	serverclock.Observe(serverNow.UTC().Format(http.TimeFormat), sent, sent)
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	created := serverNow.Add(-5*time.Minute - 30*time.Second) // Date has second resolution
	if got := relativeTimestamp(created); got != "5m ago" {
		t.Fatalf("session created 5 minutes ago on the server read %q on a machine 10 minutes ahead", got)
	}
	expires := serverNow.Add(3*time.Hour + time.Minute)
	if got := relativeTimestamp(expires); got != "in 3h" {
		t.Fatalf("an expiry 3 hours ahead read %q", got)
	}
	if got := relativeTimestamp(serverNow.Add(10 * time.Second)); got != "now" {
		t.Fatalf("an expiry in 10 seconds read %q", got)
	}
}
