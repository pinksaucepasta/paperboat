package derpquic

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

type fakeCarrier struct {
	mu         sync.Mutex
	connectErr error
	connected  bool
	closed     int
	connects   int
	wait       bool
	waitPing   bool
	terminal   bool
	recvErr    error
}

func (f *fakeCarrier) Connect(ctx context.Context) error {
	f.mu.Lock()
	f.connects++
	wait := f.wait
	err := f.connectErr
	terminal := f.terminal
	f.mu.Unlock()
	if terminal {
		return ErrClosed
	}
	if wait {
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	return nil
}
func (f *fakeCarrier) Close() error {
	f.mu.Lock()
	f.connected = false
	f.terminal = true
	f.closed++
	f.mu.Unlock()
	return nil
}
func (f *fakeCarrier) Disconnect() error {
	f.mu.Lock()
	f.connected = false
	f.closed++
	f.mu.Unlock()
	return nil
}

func TestFallbackCarrierBoundsPreferredAttemptAndSerializesConnect(t *testing.T) {
	preferred, fallback := &fakeCarrier{wait: true}, &fakeCarrier{}
	c := NewFallbackCarrier(preferred, fallback, time.Hour)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	errs := make(chan error, 2)
	go func() { errs <- c.Connect(ctx) }()
	go func() { errs <- c.Connect(ctx) }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(started)
	if elapsed < preferredAttemptTimeout || elapsed >= 3*time.Second {
		t.Fatalf("fallback selected after %v", elapsed)
	}
	preferred.mu.Lock()
	preferredConnects := preferred.connects
	preferred.mu.Unlock()
	fallback.mu.Lock()
	fallbackConnects := fallback.connects
	fallback.mu.Unlock()
	if preferredConnects != 1 || fallbackConnects != 1 {
		t.Fatalf("connect counts preferred=%d fallback=%d", preferredConnects, fallbackConnects)
	}
}

func TestFallbackCarrierCancellationInterruptsPreferredAttempt(t *testing.T) {
	preferred, fallback := &fakeCarrier{wait: true}, &fakeCarrier{}
	c := NewFallbackCarrier(preferred, fallback, time.Hour)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := c.Connect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("connect error=%v", err)
	}
	if time.Since(started) >= time.Second {
		t.Fatal("caller cancellation waited for preferred subdeadline")
	}
	fallback.mu.Lock()
	connects := fallback.connects
	fallback.mu.Unlock()
	if connects != 0 {
		t.Fatalf("fallback connects=%d after cancellation", connects)
	}
}
func (f *fakeCarrier) ready() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.connected {
		return context.DeadlineExceeded
	}
	return nil
}
func (f *fakeCarrier) Send(key.NodePublic, []byte) error        { return f.ready() }
func (f *fakeCarrier) SendControl(key.NodePublic, []byte) error { return f.ready() }
func (f *fakeCarrier) RecvDetail() (derp.ReceivedMessage, int, error) {
	f.mu.Lock()
	err := f.recvErr
	f.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	return nil, 0, f.ready()
}
func (*fakeCarrier) NotePreferred(bool)                   {}
func (f *fakeCarrier) SendPong([8]byte) error             { return f.ready() }
func (f *fakeCarrier) LocalAddr() (netip.AddrPort, error) { return netip.AddrPort{}, f.ready() }
func (f *fakeCarrier) Ping(ctx context.Context) error {
	f.mu.Lock()
	wait := f.waitPing
	f.mu.Unlock()
	if wait {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.ready()
}

func TestFallbackCarrierBoundsPreferredPingAndRecovers(t *testing.T) {
	preferred, fallback := &fakeCarrier{waitPing: true}, &fakeCarrier{}
	c := NewFallbackCarrier(preferred, fallback, 50*time.Millisecond)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < preferredAttemptTimeout || elapsed >= 4*time.Second {
		t.Fatalf("fallback ping completed after %v", elapsed)
	}
	if active, _ := c.current(); active != fallback {
		t.Fatal("failed preferred ping did not activate WSS")
	}
	preferred.mu.Lock()
	preferred.waitPing = false
	preferred.mu.Unlock()
	eventually(t, func() bool { active, _ := c.current(); return active == preferred })
	preferred.mu.Lock()
	preferred.waitPing = true
	preferred.mu.Unlock()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if active, _ := c.current(); active != fallback {
		t.Fatal("second failed preferred ping did not reactivate WSS")
	}
	fallback.mu.Lock()
	connects, terminal := fallback.connects, fallback.terminal
	fallback.mu.Unlock()
	if connects != 2 || terminal {
		t.Fatalf("fallback reconnects=%d terminal=%t", connects, terminal)
	}
}

func TestFallbackCarrierCurrentFatalReceiveRemainsTerminal(t *testing.T) {
	preferred := &fakeCarrier{connected: true, recvErr: ErrAdmission}
	c := NewFallbackCarrier(preferred, &fakeCarrier{}, time.Hour)
	defer c.Close()
	c.mu.Lock()
	c.active = preferred
	c.mu.Unlock()
	if _, _, err := c.RecvDetail(); !errors.Is(err, ErrAdmission) {
		t.Fatalf("receive error=%v", err)
	}
	if _, err := c.current(); !errors.Is(err, ErrAdmission) {
		t.Fatalf("fatal error was not latched: %v", err)
	}
}

func TestRecoveryRetryDelayBounds(t *testing.T) {
	base := 5 * time.Second
	for _, test := range []struct {
		failures uint
		minimum  time.Duration
	}{
		{failures: 0, minimum: 4 * time.Second},
		{failures: 1, minimum: 8 * time.Second},
		{failures: 20, minimum: 8 * time.Second},
	} {
		for _, random := range []uint64{0, 200, 400, ^uint64(0)} {
			delay := recoveryRetryDelay(base, test.failures, random)
			if delay < test.minimum || delay > 2*base {
				t.Fatalf("failures=%d random=%d delay=%v", test.failures, random, delay)
			}
		}
	}
}

func TestFallbackCarrierReachabilityRecoveryAndFatalDenial(t *testing.T) {
	preferred, fallback := &fakeCarrier{connectErr: context.DeadlineExceeded}, &fakeCarrier{}
	c := NewFallbackCarrier(preferred, fallback, 5*time.Millisecond)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a, _ := c.current(); a != fallback {
		t.Fatal("reachability failure did not select WSS")
	}
	preferred.mu.Lock()
	preferred.connectErr = nil
	preferred.mu.Unlock()
	eventually(t, func() bool { a, _ := c.current(); return a == preferred })
	fallback.mu.Lock()
	closed := fallback.closed
	fallback.mu.Unlock()
	if closed == 0 {
		t.Fatal("recovery did not close WSS")
	}

	deniedFallback := &fakeCarrier{}
	denied := NewFallbackCarrier(&fakeCarrier{connectErr: ErrAdmission}, deniedFallback, time.Millisecond)
	defer denied.Close()
	if err := denied.Connect(context.Background()); !errors.Is(err, ErrAdmission) {
		t.Fatalf("denial: %v", err)
	}
	deniedFallback.mu.Lock()
	connected := deniedFallback.connected
	deniedFallback.mu.Unlock()
	if connected {
		t.Fatal("authorization denial triggered weaker fallback")
	}
	canceledFallback := &fakeCarrier{}
	canceled := NewFallbackCarrier(&fakeCarrier{connectErr: context.Canceled}, canceledFallback, time.Millisecond)
	defer canceled.Close()
	canceledContext, stop := context.WithCancel(context.Background())
	stop()
	if err := canceled.Connect(canceledContext); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
	canceledFallback.mu.Lock()
	connected = canceledFallback.connected
	canceledFallback.mu.Unlock()
	if connected {
		t.Fatal("caller cancellation triggered fallback")
	}
}

func TestFallbackCarrierRecoveryRetriesAfterConnectGateContention(t *testing.T) {
	preferred, fallback := &fakeCarrier{connectErr: context.DeadlineExceeded}, &fakeCarrier{}
	c := NewFallbackCarrier(preferred, fallback, 10*time.Millisecond)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Model another bounded carrier transition holding the serialization gate
	// beyond one preferred recovery attempt. Losing this race must delay QUIC
	// recovery, not permanently terminate its only worker while WSS is active.
	c.connectGate <- struct{}{}
	preferred.mu.Lock()
	preferred.connectErr = nil
	preferred.mu.Unlock()
	time.Sleep(preferredAttemptTimeout + 100*time.Millisecond)
	releaseConnect(c.connectGate)
	eventually(t, func() bool { active, _ := c.current(); return active == preferred })
}
