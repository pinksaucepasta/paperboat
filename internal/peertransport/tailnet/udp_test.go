package tailnet

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
)

func TestUDPAdmissionAndCleanup(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	k := key.NewNode()
	server, err := ListenUDP(dm.Regions[1], []key.NodePublic{k.Public()}, 4242)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// Wrong keys cannot establish the underlying tunnel, before UDP dispatch.
	bad := &tailcat.Client{Server: server.Address(), Key: key.NewNode(), Logf: func(string, ...any) {}}
	denied, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	if c, e := bad.DialUDPPort(denied, 4242); e == nil {
		c.Close()
		t.Error("unlisted key admitted")
	}
	stop()
	bad.Close()
	raw := &tailcat.Client{Server: server.Address(), Key: k, Logf: func(string, ...any) {}}
	defer raw.Close()
	wrong, err := raw.DialUDPPort(ctx, 4243)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err = wrong.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	denied, stop = context.WithTimeout(ctx, 100*time.Millisecond)
	if p, e := server.Accept(denied); !errors.Is(e, context.DeadlineExceeded) {
		if p != nil {
			p.Close()
		}
		t.Fatalf("wrong port reached admission: %v", e)
	}
	stop()
	// Fill the real server admission queue; excess flows do not allocate a
	// handler/lease and closing one permits admission again.
	flows := make([]tailcat.ConnPacketConn, 0, MaxFlows+1)
	defer func() {
		for _, p := range flows {
			p.Close()
		}
	}()
	for range MaxFlows {
		p, e := raw.DialUDPPort(ctx, 4242)
		if e != nil {
			t.Fatal(e)
		}
		flows = append(flows, p)
		if _, e = p.Write([]byte{1}); e != nil {
			t.Fatal(e)
		}
		// Wait for each admission to distinguish capacity from delivery timing.
		deadline := time.Now().Add(time.Second)
		for len(server.ready) != len(flows) {
			if time.Now().After(deadline) {
				t.Fatal("flow not admitted")
			}
			time.Sleep(time.Millisecond)
		}
	}
	extra, e := raw.DialUDPPort(ctx, 4242)
	if e != nil {
		t.Fatal(e)
	}
	flows = append(flows, extra)
	if _, e = extra.Write([]byte{1}); e != nil {
		t.Fatal(e)
	}
	time.Sleep(50 * time.Millisecond)
	server.mu.Lock()
	count := len(server.flows)
	server.mu.Unlock()
	if count != MaxFlows || len(server.slots) != MaxFlows {
		t.Fatalf("unbounded admission: %d", count)
	}
	p, e := server.Accept(ctx)
	if e != nil {
		t.Fatal(e)
	}
	local, remote := p.LocalAddr().String(), p.RemoteAddr().String()
	// Concurrent close/read/write/deadline changes must not race or block.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_ = p.SetDeadline(time.Now().Add(time.Millisecond))
				_, _ = p.WriteTo([]byte{1}, p.RemoteAddr())
				_ = p.Close()
			}
		}()
	}
	wg.Wait()
	if p.LocalAddr().String() != local || p.RemoteAddr().String() != remote {
		t.Fatal("addresses changed after close")
	}
	if _, e = extra.Write([]byte{2}); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for len(server.slots) != MaxFlows {
		if time.Now().After(deadline) {
			t.Fatal("closed flow did not restore capacity")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	_ = server.Close()
	if time.Since(started) > 5*time.Second {
		t.Fatal("server cleanup exceeded deadline")
	}
	if len(server.slots) != 0 || len(server.ready) != 0 || len(server.flows) != 0 {
		t.Fatal("server retained socket leases")
	}
	if _, e = server.Accept(ctx); !errors.Is(e, net.ErrClosed) {
		t.Fatalf("accept after shutdown: %v", e)
	}
}

func TestUDPClientCanceledOpenAndShutdown(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	admitted := key.NewNode()
	server, err := ListenUDP(dm.Regions[1], []key.NodePublic{admitted.Public()}, 4242)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := NewUDPClient(server.Address(), key.NewNode(), 4242)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if p, e := client.Dial(ctx); e == nil {
		p.Close()
		t.Fatal("canceled open succeeded")
	}
	done := make(chan error, 3)
	for range 3 {
		go func() {
			p, e := client.Dial(t.Context())
			if p != nil {
				p.Close()
			}
			done <- e
		}()
	}
	closed := make(chan struct{})
	go func() { client.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel pending opens")
	}
	for range 3 {
		select {
		case e := <-done:
			if e == nil {
				t.Fatal("unauthorized open succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("open worker leaked")
		}
	}
	client.mu.Lock()
	count := client.count
	flows := len(client.flows)
	client.mu.Unlock()
	if count != 0 || flows != 0 {
		t.Fatalf("open reservations leaked: %d / %d", count, flows)
	}
}
