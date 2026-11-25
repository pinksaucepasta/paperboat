//go:build linux || darwin

package machineguard

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// A request that ended while queued behind another one used to close the connection, and with it every lease on it.
func TestRequestEndedWhileQueuedKeepsTheConnection(t *testing.T) {
	conn, _ := unixPair(t)
	client := &Client{conn: conn}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Status(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("status returned %v; want context.Canceled", err)
	}
	if client.conn == nil {
		t.Fatal("a request that was never sent closed the guard connection")
	}
}

// Another OS user's slow but allowed policy change holds the guard's lock past 5 s: the status ping waits behind it, and
// giving up early closed the connection, withdrew every lease and made the daemon rebuild all of them, over and over.
func TestStatusBehindASlowOperationKeepsEveryLease(t *testing.T) {
	var applies atomic.Int32
	apply := applyPolicy
	applyPolicy = func(context.Context, Config, []*guardedLease) error { applies.Add(1); return nil }
	t.Cleanup(func() { applyPolicy = apply })
	state := t.TempDir()
	recordTestDefaultDomain(t, state)
	near, far := unixPair(t)
	server := &unixControlConn{far}
	g := &guardServer{cfg: Config{StateDir: state, LoopbackCIDR: "127.100.0.0/16"}, leases: map[string]*guardedLease{},
		names: map[controlConn]map[string]string{}, connections: map[controlConn]string{server: ""}, failures: make(chan error, 1)}
	for i := range 50 {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		g.leases[strconv.Itoa(i)] = &guardedLease{listener: listener, owner: server}
	}
	g.wg.Add(1)
	go g.serveConnection(context.Background(), server)
	client := &Client{conn: near}
	g.mu.Lock()
	release := time.AfterFunc(6*time.Second, g.mu.Unlock)
	_, err := client.Status(t.Context())
	if err != nil && release.Stop() {
		g.mu.Unlock()
	}
	if closeErr := client.Close(); err == nil {
		g.wg.Wait()
		if closeErr != nil || applies.Load() != 1 { // the deliberate close's own withdrawal, nothing before it
			t.Fatalf("close: %v, policy changes %d; want one", closeErr, applies.Load())
		}
		return
	}
	g.wg.Wait()
	t.Fatalf("status: %v; leases left %d of 50, policy changes %d", err, len(g.leases), applies.Load())
}
