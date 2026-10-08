package connector

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestRegionalCarriersStandbyRepairAndConcurrentIngress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := DefaultDataCarrierPoolConfig()
	config.Targets = []DataCarrierTarget{{EdgeID: "edge-a", ProcessEpoch: "epoch-a", FailureDomain: "domain-a"}, {EdgeID: "edge-b", ProcessEpoch: "epoch-b", FailureDomain: "domain-b"}}
	config.Carrier = testDataCarrierConfig()
	var mu sync.Mutex
	failed := true
	desired := append([]DataCarrierTarget(nil), config.Targets...)
	config.TargetsExpireAt = time.Now().Add(15 * time.Second)
	config.RefreshTargets = func(context.Context) ([]DataCarrierTarget, time.Time, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]DataCarrierTarget(nil), desired...), time.Now().Add(15 * time.Second), nil
	}
	servers := map[string]*DataCarrier{}
	dialer := func(ctx context.Context, r DataCarrierDialRequest) (DataCarrierDialResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.EdgeID == "edge-a" && failed {
			return DataCarrierDialResult{}, errors.New("primary unavailable")
		}
		local, remote := net.Pipe()
		server, err := NewDataCarrierServer(ctx, remote, config.Carrier, DataCarrierAdmission{Identity: config.Session, Authorize: func(context.Context, StreamOpen) error { return nil }})
		if err != nil {
			local.Close()
			remote.Close()
			return DataCarrierDialResult{}, err
		}
		if old := servers[r.EdgeID]; old != nil {
			old.Close()
		}
		servers[r.EdgeID] = server
		return DataCarrierDialResult{Link: local, PeerIdentity: config.Session, Transport: r.Transport, EdgeID: r.EdgeID, FailureDomain: r.FailureDomain}, nil
	}
	// Dial context is bounded to establishment; sessions have the pool lifetime.
	wrapped := func(_ context.Context, r DataCarrierDialRequest) (DataCarrierDialResult, error) {
		return dialer(ctx, r)
	}
	pool, err := NewDataCarrierPool(ctx, config, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		pool.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, s := range servers {
			s.Close()
		}
	}()
	if err := pool.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pool.Snapshot(); len(got) != 1 || got[0].EdgeID != "edge-b" {
		t.Fatalf("standby unavailable: %+v", got)
	}
	mu.Lock()
	failed = false
	mu.Unlock()
	deadline := time.Now().Add(4 * time.Second)
	for len(pool.Snapshot()) != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(pool.Snapshot()) != 2 {
		t.Fatal("missing primary was not repaired")
	}
	// Both streams arrive before the first pool accept, reproducing lost fan-in results.
	var opened []io.ReadWriteCloser
	mu.Lock()
	for _, id := range []string{"edge-a", "edge-b"} {
		s, err := servers[id].OpenStream(ctx)
		if err != nil {
			mu.Unlock()
			t.Fatal(err)
		}
		opened = append(opened, s)
	}
	mu.Unlock()
	defer func() {
		for _, s := range opened {
			s.Close()
		}
	}()
	seen := map[string]bool{}
	for range 2 {
		s, _, err := pool.AcceptStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		target := s.EdgeTarget()
		if seen[target.EdgeID] || target.ProcessEpoch == "" {
			t.Fatalf("invalid source: %+v", target)
		}
		seen[target.EdgeID] = true
		s.Close()
	}
	if !seen["edge-a"] || !seen["edge-b"] {
		t.Fatalf("lost simultaneous ingress: %v", seen)
	}
	mu.Lock()
	desired[0].ProcessEpoch = "epoch-replacement"
	mu.Unlock()
	replaced := false
	for !replaced && ctx.Err() == nil {
		for _, info := range pool.Snapshot() {
			if info.EdgeID == "edge-a" && info.ProcessEpoch == "epoch-replacement" {
				replaced = true
			}
		}
		if !replaced {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !replaced {
		t.Fatal("fresh placement did not replace old process")
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, _, err := pool.AcceptStream(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestRegionalCarrierExpiredDescriptorNeverDials(t *testing.T) {
	config := DefaultDataCarrierPoolConfig()
	config.MaximumCarriers = 1
	config.Targets = []DataCarrierTarget{{EdgeID: "edge-a", ProcessEpoch: "epoch-a", FailureDomain: "domain-a"}}
	config.TargetsExpireAt = time.Now().Add(-time.Second)
	calls := 0
	pool, err := NewDataCarrierPool(context.Background(), config, func(context.Context, DataCarrierDialRequest) (DataCarrierDialResult, error) {
		calls++
		return DataCarrierDialResult{}, errors.New("unexpected dial")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Connect(context.Background()); !errors.Is(err, ErrDataCarrierUnavailable) {
		t.Fatalf("expired authority: %v", err)
	}
	if calls != 0 {
		t.Fatal("expired descriptor was used for connection")
	}
}

// The session keeps its transport open while withholding authenticated ping
// replies, modeling a silent peer rather than an explicit connection close.
type regionalSilentSession struct {
	mu      sync.Mutex
	replies []bool
	calls   int
	done    chan struct{}
	once    sync.Once
}

func (s *regionalSilentSession) OpenStream(context.Context) (DataCarrierStreamLink, error) {
	return nil, ErrDataCarrierUnavailable
}
func (s *regionalSilentSession) AcceptStream(ctx context.Context) (DataCarrierStreamLink, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, ErrDataCarrierClosed
	}
}
func (s *regionalSilentSession) Ping(ctx context.Context) error {
	s.mu.Lock()
	i := s.calls
	s.calls++
	reply := i < len(s.replies) && s.replies[i]
	s.mu.Unlock()
	if reply {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrDataCarrierClosed
	}
}
func (s *regionalSilentSession) Close() error               { s.once.Do(func() { close(s.done) }); return nil }
func (s *regionalSilentSession) CloseChan() <-chan struct{} { return s.done }

func TestRegionalCarrierSilentPeerMissesResetAndExposeSurvivor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	config := DefaultDataCarrierPoolConfig()
	pool, err := NewDataCarrierPool(ctx, config, func(context.Context, DataCarrierDialRequest) (DataCarrierDialResult, error) {
		return DataCarrierDialResult{}, ErrDataCarrierUnavailable
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	silent := &regionalSilentSession{done: make(chan struct{}), replies: []bool{false, false, true, false, false, false}}
	survivor := &regionalSilentSession{done: make(chan struct{})}
	first, err := NewDataCarrierClientWithSession(ctx, silent, config.Carrier, config.Session)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDataCarrierClientWithSession(ctx, survivor, config.Carrier, config.Session)
	if err != nil {
		t.Fatal(err)
	}
	pool.carriers = []*pooledDataCarrier{{carrier: first, edgeID: "edge-a"}, {carrier: second, edgeID: "edge-b"}}
	pool.state = DataCarrierPoolReady
	pool.workers.Add(1)
	go pool.monitorCarrier(first, 20*time.Millisecond, 5*time.Millisecond)
	select {
	case <-first.Done():
	case <-ctx.Done():
		t.Fatal("silent open peer stayed ready")
	}
	pool.workers.Wait()
	silent.mu.Lock()
	calls := silent.calls
	silent.mu.Unlock()
	if calls != 6 {
		t.Fatalf("response did not reset missed count: ping calls=%d", calls)
	}
	snapshot := pool.Snapshot()
	if len(snapshot) != 1 || snapshot[0].EdgeID != "edge-b" || snapshot[0].State == DataCarrierClosed {
		t.Fatalf("surviving carrier not observable: %+v", snapshot)
	}
	if pool.State() != DataCarrierPoolReady {
		t.Fatal("healthy survivor lost readiness")
	}
}
