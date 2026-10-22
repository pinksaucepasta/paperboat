package datacarrier

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// delayedConn delivers every write to its peer after a fixed one-way delay, in order.
type delayedConn struct {
	net.Conn
	writes chan []byte
}

func withDelay(t *testing.T, conn net.Conn, delay time.Duration) *delayedConn {
	type timed struct {
		at   time.Time
		data []byte
	}
	d := &delayedConn{Conn: conn, writes: make(chan []byte, 4096)}
	queue := make(chan timed, 4096)
	go func() {
		defer close(queue)
		for data := range d.writes {
			queue <- timed{time.Now().Add(delay), data}
		}
	}()
	go func() {
		for item := range queue {
			time.Sleep(time.Until(item.at))
			if _, err := conn.Write(item.data); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { close(d.writes); conn.Close() })
	return d
}

func (d *delayedConn) Write(p []byte) (int, error) {
	d.writes <- append([]byte(nil), p...)
	return len(p), nil
}

// One stream must not be held to its initial window per round trip: with the window pinned
// at 256 KiB, 16 MiB at 50 ms RTT needs at least 64 round trips (3.2 s).
func TestCarrierStreamWindowGrowsBeyondInitialAtRealRTT(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	serverConn := <-accepted
	config := testCarrierConfig(4)
	config.KeepAliveInterval = time.Second
	client, err := newYamuxSession(withDelay(t, clientConn, 25*time.Millisecond), config, true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := newYamuxSession(withDelay(t, serverConn, 25*time.Millisecond), config, false)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const size = 16 << 20
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	go func() {
		stream, err := server.AcceptStream(ctx)
		if err != nil {
			return
		}
		defer stream.Close()
		buffer := make([]byte, 64<<10)
		for sent := 0; sent < size; sent += len(buffer) {
			if _, err := stream.Write(buffer); err != nil {
				return
			}
		}
	}()
	stream, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Write([]byte{1}); err != nil { // the opener speaks first
		t.Fatal(err)
	}
	started := time.Now()
	if n, err := io.CopyN(io.Discard, stream, size); err != nil || n != size {
		t.Fatalf("read %d of %d: %v", n, size, err)
	}
	elapsed := time.Since(started)
	t.Logf("16 MiB at 50 ms RTT in %v (%.0f Mbit/s)", elapsed, size*8/elapsed.Seconds()/1e6)
	if elapsed > 1600*time.Millisecond {
		t.Fatalf("16 MiB took %v at 50 ms RTT: the stream window did not grow beyond its initial size", elapsed)
	}
}
