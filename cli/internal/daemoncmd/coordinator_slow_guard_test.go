package daemoncmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
	"github.com/pinksaucepasta/paperboat/cli/internal/testcert"
)

// A slow guard that, like the real client, loses its connection (and every listener) when a context cuts a call.
type slowGuardClient struct {
	*coordinatorNameClient
	delay, gatewayDelay time.Duration
	mu                  sync.Mutex
	dropped             bool
	acquires            map[string]int
	listeners           map[string]net.Listener
}

func (c *slowGuardClient) call(ctx context.Context, delay time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped {
		return net.ErrClosed
	}
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		c.dropped = true
		return ctx.Err()
	}
}

func (c *slowGuardClient) AcquireMachine(ctx context.Context, account, machine, hostname string, ip netip.Addr, port int) (net.Listener, netip.Addr, error) {
	if err := c.call(ctx, c.delay); err != nil {
		return nil, netip.Addr{}, err
	}
	listener, assigned, err := c.coordinatorNameClient.AcquireMachine(ctx, account, machine, hostname, ip, port)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acquires[machine]++
	c.listeners[machine] = listener
	return listener, assigned, err
}

func (c *slowGuardClient) Acquire(ctx context.Context, host string, ip netip.Addr, port int) (net.Listener, error) {
	if port >= 9000 {
		if err := c.call(ctx, c.gatewayDelay); err != nil {
			return nil, err
		}
	}
	return c.coordinatorNameClient.Acquire(ctx, host, ip, port)
}

func (c *slowGuardClient) ApplyPolicy(ctx context.Context) error {
	if err := c.call(ctx, 0); err != nil {
		return err
	}
	return c.coordinatorNameClient.ApplyPolicy(ctx)
}

func slowGuardCoordinator(t *testing.T, delay, gatewayDelay time.Duration, certificates bool) (*Coordinator, *slowGuardClient, *atomic.Int32) {
	t.Helper()
	client := &slowGuardClient{coordinatorNameClient: &coordinatorNameClient{}, delay: delay, gatewayDelay: gatewayDelay,
		acquires: map[string]int{}, listeners: map[string]net.Listener{}}
	dials := &atomic.Int32{}
	cfg := CoordinatorConfig{
		SocketAddress: socketAddress(shorttemp.Dir(t)), RetryBase: time.Hour, RetryMax: time.Hour,
		ReconcileTimeout:  200 * time.Millisecond, // far below every setup here: it bounds one step, not the setup
		ConnectNameClient: func(context.Context) (guardedNameClient, error) { dials.Add(1); return client, nil },
		DialMachine:       func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unexpected dial") },
	}
	if certificates {
		ca, err := testcert.New()
		if err != nil {
			t.Fatal(err)
		}
		cert, key, err := ca.IssueCertificate([]string{"example.test"})
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			t.Fatal(err)
		}
		cfg.IssueCertificate = func(context.Context, string, string, int) (tls.Certificate, error) { return pair, nil }
	}
	c, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	return c, client, dials
}

func slowGuardPeers(n int) (peers []*daemonrpc.PeerUpdate) {
	for i := range n {
		peers = append(peers, &daemonrpc.PeerUpdate{PeerId: fmt.Sprintf("machine-%d", i), Alias: fmt.Sprintf("box-%d", i),
			AssignedIp: fmt.Sprintf("127.100.%d.%d", 1+i/200, 1+i%200), Approved: true, Online: true, ExportedPorts: []int32{8080}})
	}
	return peers
}

// Triggers with short deadlines keep arriving while a setup far longer than any of them runs: it is done exactly once.
func TestCoordinatorSlowSetupRunsOnceWhileTriggersArrive(t *testing.T) {
	for _, machines := range []int{179, 1000} {
		t.Run(fmt.Sprint(machines), func(t *testing.T) {
			c, client, dials := slowGuardCoordinator(t, time.Millisecond, 0, false)
			peers := slowGuardPeers(machines)
			stop := make(chan struct{})
			var triggers sync.WaitGroup
			for range 3 {
				triggers.Go(func() {
					for {
						select {
						case <-stop:
							return
						case <-time.After(5 * time.Millisecond):
						}
						ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
						_ = c.reconcileSnapshot(ctx, peers, 1)
						cancel()
					}
				})
			}
			setup := c.queueSnapshot(peers, 1, nil)
			var longest time.Duration // what pb status, doctor and the RPC read while the setup runs
			for done := false; !done; {
				started := time.Now()
				_, _ = c.PendingLocalAccessStep(), c.PendingLocalAccessDetail()
				_, _ = c.rpcBackend.GetStatus(t.Context())
				longest = max(longest, time.Since(started))
				select {
				case <-setup.done:
					done = true
				case <-time.After(time.Millisecond):
				}
			}
			err := setup.wait(t.Context())
			t.Logf("longest reader wait %v", longest)
			if longest > time.Second {
				t.Fatalf("a reader waited %v for the setup", longest)
			}
			close(stop)
			triggers.Wait()
			if err != nil || dials.Load() != 1 {
				t.Fatalf("err=%v dials=%d; want one setup on one connection", err, dials.Load())
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			for machine, count := range client.acquires {
				if count != 1 {
					t.Fatalf("%s acquired %d times", machine, count)
				}
			}
			if len(client.acquires) != machines {
				t.Fatalf("%d of %d machines acquired", len(client.acquires), machines)
			}
		})
	}
}

// A caller's deadline in the middle of the browser gateway's port listeners cuts nothing (apple, 2026-11-24).
func TestCoordinatorSlowGatewayPortsSurviveACallersDeadline(t *testing.T) {
	c, _, dials := slowGuardCoordinator(t, 0, 10*time.Millisecond, true)
	peer := &daemonrpc.PeerUpdate{PeerId: "machine", Alias: "laptop", AssignedIp: "127.100.1.1", Approved: true, Online: true}
	for i := range 80 {
		peer.ExportedPorts = append(peer.ExportedPorts, int32(9000+i))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := c.reconcileSnapshot(ctx, []*daemonrpc.PeerUpdate{peer}, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v; want the caller's deadline", err)
	}
	select { // the inventory, which gave up, hears that the setup ended instead of waiting for its next refresh
	case <-c.LocalAccessChanged():
	case <-time.After(5 * time.Second):
		t.Fatal("no change signal after the abandoned setup ended")
	}
	if err := c.reconcileSnapshot(t.Context(), []*daemonrpc.PeerUpdate{peer}, 1); err != nil || dials.Load() != 1 {
		t.Fatalf("err=%v dials=%d; want a finished setup on one connection", err, dials.Load())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, web := range c.webRoutes {
		if len(web.ports) != 80 {
			t.Fatalf("%d of 80 browser port listeners", len(web.ports))
		}
	}
}

// A snapshot that withdraws a machine during a long setup closes its listener before the setup goes on, and the other
// listeners stay.
func TestCoordinatorWithdrawalDuringSlowSetupTakesEffectFirst(t *testing.T) {
	change := map[string]func(*daemonrpc.PeerUpdate){
		"revoked": func(p *daemonrpc.PeerUpdate) { p.Approved = false },
		"offline": func(p *daemonrpc.PeerUpdate) { p.Online = false },
		"port":    func(p *daemonrpc.PeerUpdate) { p.ExportedPorts = []int32{8081} },
		"renamed": func(p *daemonrpc.PeerUpdate) { p.Alias = "other" },
		"moved":   func(p *daemonrpc.PeerUpdate) { p.AssignedIp = "127.100.9.9" },
	}
	for name, apply := range change {
		t.Run(name, func(t *testing.T) {
			c, client, dials := slowGuardCoordinator(t, 2*time.Millisecond, 0, false)
			peers := slowGuardPeers(400)
			first := c.queueSnapshot(peers, 1, nil)
			held := func(machine string) net.Listener {
				client.mu.Lock()
				defer client.mu.Unlock()
				return client.listeners[machine]
			}
			for held("machine-5") == nil {
				time.Sleep(time.Millisecond)
			}
			withdrawn := held("machine-0")
			changed := slowGuardPeers(400)
			apply(changed[0])
			second := c.queueSnapshot(changed, 2, nil)
			closed := make(chan struct{})
			go func() { _, _ = withdrawn.Accept(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(300 * time.Millisecond): // the rest of the setup takes 800 ms
				t.Fatal("the withdrawn machine's listener stayed open during the setup")
			}
			if err := first.wait(t.Context()); !errors.Is(err, errSuperseded) {
				t.Fatalf("first setup err=%v", err)
			}
			if err := second.wait(t.Context()); err != nil || dials.Load() != 1 {
				t.Fatalf("second setup err=%v dials=%d", err, dials.Load())
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if client.acquires["machine-3"] != 1 {
				t.Fatalf("an unaffected machine was acquired %d times", client.acquires["machine-3"])
			}
		})
	}
}

// Stop ends a running setup at once.
func TestCoordinatorStopEndsASlowSetup(t *testing.T) {
	c, _, _ := slowGuardCoordinator(t, 50*time.Millisecond, 0, false)
	request := c.queueSnapshot(slowGuardPeers(200), 1, nil)
	time.Sleep(100 * time.Millisecond)
	started := time.Now()
	_ = c.Stop()
	if err := request.wait(t.Context()); err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("err=%v after %v; want the setup ended at once", err, time.Since(started))
	}
}
