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
	// A healthy pool re-reads placement when the descriptor nears expiry (targetRefreshMargin), so the
	// descriptor lives 3 s here for the replacement below to be seen within the test.
	config.TargetsExpireAt = time.Now().Add(3 * time.Second)
	config.RefreshTargets = func(context.Context) ([]DataCarrierTarget, time.Time, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]DataCarrierTarget(nil), desired...), time.Now().Add(3 * time.Second), nil
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
	// clock, when set, moves by the ping's entry in elapse while that ping is in flight: the monitor reads
	// the time before and after its ping inside one iteration, so only the monitor's own call moves it.
	clock  *steppedHealthClock
	elapse []time.Duration
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
	if s.clock != nil && i < len(s.elapse) {
		s.clock.advance(s.elapse[i])
	}
	s.mu.Unlock()
	if reply {
		return nil
	}
	return context.DeadlineExceeded // no answer before its deadline; the stepped clock says how long that took
}
func (s *regionalSilentSession) Close() error               { s.once.Do(func() { close(s.done) }); return nil }
func (s *regionalSilentSession) CloseChan() <-chan struct{} { return s.done }

// steppedHealthClock moves only while a ping is in flight (regionalSilentSession.elapse) and delivers a tick
// only when the monitor is back in its select, so every decision reads one fixed time before and one after
// its ping, whatever the runner's scheduling (macOS runs 41993423157 and 42031541678 closed at ping 5 and 2).
type steppedHealthClock struct {
	mu    sync.Mutex
	now   time.Time
	ticks chan time.Time
}

func (c *steppedHealthClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *steppedHealthClock) NewTicker(time.Duration) (<-chan time.Time, func()) {
	return c.ticks, func() {}
}
func (c *steppedHealthClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// monitorSilentPeer runs the pool's health monitor over a peer answering only the pings marked true, each
// ping taking its elapse entry of clock time, and returns how many pings it sent before closing.
func monitorSilentPeer(t *testing.T, replies []bool, elapse []time.Duration, interval time.Duration) (int, *DataCarrierPool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	config := DefaultDataCarrierPoolConfig()
	pool, err := NewDataCarrierPool(ctx, config, func(context.Context, DataCarrierDialRequest) (DataCarrierDialResult, error) {
		return DataCarrierDialResult{}, ErrDataCarrierUnavailable
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	// An hour ahead: probe deadlines from this clock stay in the real future.
	clock := &steppedHealthClock{now: time.Now().Add(time.Hour), ticks: make(chan time.Time)}
	silent := &regionalSilentSession{done: make(chan struct{}), replies: replies, clock: clock, elapse: elapse}
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
	pool.healthClock = clock
	pool.workers.Add(1)
	go pool.monitorCarrier(first, interval, interval/4)
	// One tick per ping; the unbuffered send waits until the previous ping's decision is done.
	for range len(elapse) + 1 {
		select {
		case clock.ticks <- clock.Now():
			continue
		case <-first.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("the health monitor stopped taking ticks")
		}
		break
	}
	select {
	case <-first.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("silent open peer stayed ready")
	}
	pool.workers.Wait()
	if reason := first.CloseReason(); reason != CloseHealthPingMissed {
		t.Fatalf("silent peer closed for %q, want %q", reason, CloseHealthPingMissed)
	}
	silent.mu.Lock()
	defer silent.mu.Unlock()
	return silent.calls, pool
}

// A response resets the miss count: with pings on time, a peer that answers its third ping is closed only
// on the third miss after it (ping 6); without the reset its first miss after the answer (ping 4) closes it.
func TestRegionalCarrierSilentPeerMissesResetAndExposeSurvivor(t *testing.T) {
	interval := time.Second
	onTime := []time.Duration{interval, interval, interval, interval, interval, interval, interval}
	calls, pool := monitorSilentPeer(t, []bool{false, false, true, false, false, false}, onTime, interval)
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

// A silent peer is closed once three intervals pass without a response, even after fewer than three
// misses: a monitor stalled past that window (a starved runner) still closes it on its next ping.
func TestRegionalCarrierSilentPeerIsClosedOnStaleness(t *testing.T) {
	interval := time.Second
	// The second ping returns three intervals after the first: a stall, as on a starved runner.
	calls, _ := monitorSilentPeer(t, nil, []time.Duration{interval, 3 * interval, interval}, interval)
	if calls != 2 {
		t.Fatalf("stale silent peer closed after %d pings, want on the second (three intervals without a response)", calls)
	}
}

// A single-edge pool (a preview or agent runtime carrier) stamps its edge process epoch on every
// accepted stream: ingress authority requires the stream's edge target to equal the decision's edge
// node and epoch, so a missing epoch refused every restricted browser stream (F7-03).
func TestSingleEdgePoolStreamsCarryTheEdgeProcessEpoch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := DefaultDataCarrierPoolConfig()
	config.MaximumCarriers, config.EdgeID, config.EdgeProcessEpoch, config.FailureDomains = 1, "edge-a", "epoch-a", []string{"edge-a"}
	config.Carrier = testDataCarrierConfig()
	var server *DataCarrier
	var mu sync.Mutex
	dialer := func(_ context.Context, r DataCarrierDialRequest) (DataCarrierDialResult, error) {
		local, remote := net.Pipe()
		s, err := NewDataCarrierServer(ctx, remote, config.Carrier, DataCarrierAdmission{Identity: config.Session, Authorize: func(context.Context, StreamOpen) error { return nil }})
		if err != nil {
			local.Close()
			remote.Close()
			return DataCarrierDialResult{}, err
		}
		mu.Lock()
		server = s
		mu.Unlock()
		return DataCarrierDialResult{Link: local, PeerIdentity: config.Session, Transport: r.Transport, EdgeID: r.EdgeID, FailureDomain: r.FailureDomain}, nil
	}
	pool, err := NewDataCarrierPool(ctx, config, dialer)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	opened, err := server.OpenStream(ctx)
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	defer server.Close()
	stream, _, err := pool.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if target := stream.EdgeTarget(); target.EdgeID != "edge-a" || target.ProcessEpoch != "epoch-a" {
		t.Fatalf("accepted stream edge target = %+v, want edge-a/epoch-a", target)
	}
}
