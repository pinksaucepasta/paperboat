package derpquic

import (
	"context"
	"net"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// A relay restarted inside an operator drain applies the start lease's drain before Serve: it binds and serves (so the
// process starts), never reports ready, and admits no client, neither before nor after a later lease repeats the drain.
func TestDrainBeforeServeAdmitsNoClient(t *testing.T) {
	f := newFixture(t)
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Minute)
	if err = s.DrainBeforeServe(deadline); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, socket, f.tls) }()
	t.Cleanup(func() {
		cancel()
		s.Close()
		select {
		case <-done:
		case <-time.After(waitBound):
			t.Error("Serve did not stop")
		}
		s.Wait()
		socket.Close()
	})
	eventually(t, s.Serving)
	if s.Ready() {
		t.Fatal("a relay started drained reports ready")
	}
	refused := func(stage string) {
		t.Helper()
		cert := certificate(t)
		c := f.client(socket.LocalAddr().String(), cert, f.grant(key.NewNode().Public(), cert, "a"))
		connectCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := c.Connect(connectCtx); err == nil {
			t.Fatalf("%s: a client was admitted by a drained relay", stage)
		}
		if st := s.Snapshot(); st.Connections != 0 || !st.Draining {
			t.Fatalf("%s: connections %d draining %v, want 0 and draining", stage, st.Connections, st.Draining)
		}
	}
	refused("before the first heartbeat")
	s.Drain(deadline) // the first heartbeat's lease repeats the drain
	refused("after the first heartbeat")
	if err := s.DrainBeforeServe(deadline); err == nil {
		t.Fatal("DrainBeforeServe after Serve bound must refuse")
	}
}
