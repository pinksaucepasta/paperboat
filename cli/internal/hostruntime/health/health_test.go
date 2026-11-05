package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestSnapshotSeparatesLivenessFromCapabilityReadiness(t *testing.T) {
	when := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	r := New("1.0.0", []string{"terminal.v1", "health.v1"}, fixedClock{when})
	r.Set("health.v1", Ready, "", 0)
	if got := r.Snapshot(); !got.Live {
		t.Fatal("capability startup must not make process dead")
	}
	r.Set("terminal.v1", Ready, "", 0)
	got := r.Snapshot()
	if !got.Live || !got.CheckedAt.Equal(when) {
		t.Fatalf("snapshot = %#v", got)
	}
	r.SetLive(false)
	if r.Snapshot().Live {
		t.Fatal("explicit shutdown must be non-live")
	}
}

func TestRetryRecoversAFailedStepWithoutARestart(t *testing.T) {
	r := New("1.0.0", nil, nil)
	attempts := 0
	var during Capability
	err := r.Retry(context.Background(), "register_network", time.Millisecond, 4*time.Millisecond, func(context.Context) error {
		attempts++
		if attempts == 1 {
			return errors.New("dial tcp: connection refused")
		}
		during = r.Snapshot().Capabilities["register_network"]
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("Retry = %v after %d attempts, want recovery on the second", err, attempts)
	}
	if during.State != Degraded || during.Reason != "dial tcp: connection refused" || during.RetryAfterMs > 1 {
		t.Fatalf("state while retrying = %#v, want degraded with the cause and the backoff", during)
	}
	if got := r.Snapshot().Capabilities["register_network"]; got.State != Ready || got.Reason != "" {
		t.Fatalf("state after recovery = %#v, want ready", got)
	}
}

func TestRetryStopsWithItsContextAndRedactsTheCause(t *testing.T) {
	r := New("1.0.0", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := r.Retry(ctx, "machine_services", time.Hour, time.Hour, func(context.Context) error {
		return errors.New("refresh failed: token=abc123secret")
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Retry = %v, want the context's end", err)
	}
	got := r.Snapshot().Capabilities["machine_services"]
	if got.State != Degraded || strings.Contains(got.Reason, "abc123secret") {
		t.Fatalf("state = %#v, want degraded with the secret redacted", got)
	}
}
