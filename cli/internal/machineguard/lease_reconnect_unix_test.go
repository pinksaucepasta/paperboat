//go:build linux || darwin

package machineguard

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// A daemon that reconnects to the guard can ask for its listener before the guard has withdrawn its old,
// dead connection: the acquire waits for that withdrawal instead of refusing, and a live owner is still refused.
func TestReconnectedAcquireWaitsForTheOldConnectionsWithdrawal(t *testing.T) {
	state := t.TempDir()
	recordTestDefaultDomain(t, state)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	old, reconnected := &namesTestConn{id: 1}, &namesTestConn{id: 2}
	lease := &guardedLease{uid: "1000", ip: "127.100.0.78", port: 8080, hostname: "laptop." + testBrowserDomain, listener: listener, owner: old}
	g := &guardServer{
		cfg:         Config{StateDir: state, LoopbackCIDR: "127.100.0.0/16"},
		leases:      map[string]*guardedLease{"127.100.0.78:8080": lease},
		connections: map[controlConn]string{old: "1000", reconnected: "1000"},
		departed:    map[controlConn]chan struct{}{old: make(chan struct{})},
	}
	in := request{Operation: "acquire", Hostname: lease.hostname, IP: lease.ip, Port: lease.port}

	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	g.mu.Lock()
	_, err = g.handle(short, reconnected, "1000", in)
	g.mu.Unlock()
	if !errors.Is(err, ErrListenerRefused) {
		t.Fatalf("acquire against a live owner: %v; want a refusal", err)
	}

	type result struct {
		listener net.Listener
		err      error
	}
	got := make(chan result, 1)
	go func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		l, err := g.handle(t.Context(), reconnected, "1000", in)
		got <- result{l, err}
	}()
	time.Sleep(20 * time.Millisecond) // the acquire is waiting, with g.mu released
	g.mu.Lock()
	lease.owner = nil // the old connection's withdrawal, as a guard handing the socket on keeps it
	close(g.departed[old])
	delete(g.departed, old)
	g.mu.Unlock()
	r := <-got
	if r.err != nil || r.listener != listener || lease.owner != controlConn(reconnected) {
		t.Fatalf("reconnected acquire: %v %v; want the listener", r.listener, r.err)
	}
}
