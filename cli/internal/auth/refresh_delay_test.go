package auth

import (
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"
)

func TestRefreshDelayCapsAtThirtySecondsWhenPaperboatIsUnreachable(t *testing.T) {
	unreachable := fmt.Errorf("refresh Paperboat session: %w", &url.Error{Op: "Post", URL: "https://example.test", Err: errors.New("i/o timeout")})
	delay := time.Duration(0)
	for _, want := range []time.Duration{15 * time.Second, 30 * time.Second, 30 * time.Second} {
		if delay = nextRefreshDelay(delay, unreachable); delay != want {
			t.Fatalf("unreachable delay %v, want %v", delay, want)
		}
	}
}

func TestRefreshDelayStillBacksOffToFiveMinutesOnServerErrors(t *testing.T) {
	refused := errors.New("refresh Paperboat session: server answered 503")
	delay := time.Duration(0)
	for _, want := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		if delay = nextRefreshDelay(delay, refused); delay != want {
			t.Fatalf("server error delay %v, want %v", delay, want)
		}
	}
}
