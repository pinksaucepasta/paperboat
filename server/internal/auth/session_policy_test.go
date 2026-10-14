package auth

import (
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"testing"
	"time"
)

func TestSessionRotationUsesConfiguredBoundary(t *testing.T) {
	s := NewService(nil, nil, nil, nil, true)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	p := config.DefaultBrowserSessions()
	p.LoginLifetime = 2 * time.Hour
	p.LoginRotateBefore = 10 * time.Minute
	if err := s.ConfigureSessions(p); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		expiry time.Time
		want   bool
	}{{time.Time{}, false}, {now.Add(10 * time.Minute), false}, {now.Add(10*time.Minute - time.Second), true}, {now.Add(time.Hour), false}} {
		if got := s.ShouldRotate(Session{ExpiresAt: tc.expiry}); got != tc.want {
			t.Fatalf("rotate=%v want=%v", got, tc.want)
		}
	}
	p.LoginLifetime = 0
	if err := s.ConfigureSessions(p); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if s.sessionPolicy.LoginLifetime != 2*time.Hour {
		t.Fatal("failed configuration replaced working policy")
	}
}
