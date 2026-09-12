package inspector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentReplayDuplicateWaitsForSameOutcome(t *testing.T) {
	_, m, id := replayTestSetup(t, ResourcePolicy{}, false)
	req := replayTestRequest(id, time.Now().UTC())
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	forward := func(context.Context, string, string, http.Header, []byte) (int, http.Header, []byte, bool, error) {
		calls.Add(1)
		close(entered)
		<-release
		return 0, nil, nil, false, errors.New("synthetic disconnect")
	}
	type outcome struct {
		result ReplayResult
		err    error
	}
	done := make(chan outcome, 2)
	go func() { r, e := m.Replay(context.Background(), req, forward); done <- outcome{r, e} }()
	<-entered
	go func() { r, e := m.Replay(context.Background(), req, forward); done <- outcome{r, e} }()
	close(release)
	a, b := <-done, <-done
	if calls.Load() != 1 || a.result.OperationID == "" || a.result.OperationID != b.result.OperationID || !errors.Is(a.err, ErrAmbiguousReplay) || !errors.Is(b.err, ErrAmbiguousReplay) {
		t.Fatalf("duplicate outcome mismatch: calls=%d errors=%v/%v", calls.Load(), a.err, b.err)
	}
	if len(m.Audit()) != 1 {
		t.Fatal("duplicate audit entries")
	}
}

func TestReplayReservesLastAuditSlotBeforeDispatch(t *testing.T) {
	s, m, id := replayTestSetup(t, ResourcePolicy{}, false)
	now := time.Now().UTC()
	for i := 0; i < ReplayAuditMaxEntries-1; i++ {
		m.ops[fmt.Sprintf("old-%d", i)] = &storedReplayOp{finished: true, expiresAt: now.Add(time.Minute)}
	}
	_ = s.SetPolicy("tunnel_02", testPolicy())
	p, _ := s.TryBegin("tunnel_02")
	rec, err := s.Finish(p, completeJSON("GET", "http://example.test/", nil, nil, []byte("GET / HTTP/1.1\r\n\r\n"), now))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := m.Replay(context.Background(), replayTestRequest(id, now), func(context.Context, string, string, http.Header, []byte) (int, http.Header, []byte, bool, error) {
			close(entered)
			<-release
			return 200, nil, nil, false, nil
		})
		done <- e
	}()
	<-entered
	req := replayTestRequest(rec.ID, now)
	req.ResourceID = "tunnel_02"
	req.IdempotencyKey = "second"
	_, err = m.Replay(context.Background(), req, func(context.Context, string, string, http.Header, []byte) (int, http.Header, []byte, bool, error) {
		t.Error("dispatched without audit capacity")
		return 200, nil, nil, false, nil
	})
	close(release)
	if !errors.Is(err, ErrNoAuditCapacity) {
		t.Fatalf("capacity error=%v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayIdleRetentionAndShutdown(t *testing.T) {
	m := NewManager(NewStore())
	expiry := time.Now().Add(25 * time.Millisecond)
	m.ops["old"] = &storedReplayOp{finished: true, expiresAt: expiry}
	m.audit = []AuditEntry{{OccurredAt: expiry.Add(-Retention)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		empty := len(m.ops) == 0 && len(m.audit) == 0
		m.mu.Unlock()
		if empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle replay retention did not expire")
		}
		time.Sleep(time.Millisecond)
	}
	m.mu.Lock()
	m.ops["new"] = &storedReplayOp{finished: true, expiresAt: time.Now().Add(Retention)}
	m.audit = []AuditEntry{{OccurredAt: time.Now()}}
	m.mu.Unlock()
	cancel()
	<-done
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ops) != 0 || len(m.audit) != 0 {
		t.Fatal("shutdown retained replay outcomes")
	}
}
