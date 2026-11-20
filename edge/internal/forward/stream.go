package forward

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-yamux/v5"
)

// Stream kinds: the first byte of every stream a link carries.
const (
	streamWatch   byte = 'W' // the opener streams what it holds (nodemesh.Watch frames)
	streamForward byte = 'F' // one forwarded visitor: Open, OpenAck, then raw bytes both ways
)

// halfCloseGrace is how long the second direction of a forwarded connection may stay open once the first ended.
const halfCloseGrace = 30 * time.Second

// streamConn adapts a yamux stream to net.Conn. Its Write waits for gate (closed once the holder has answered the
// Open) so the listener that serves it cannot write before the forwarder has its ack.
type streamConn struct {
	*yamux.Stream
	local, remote net.Addr
	release       func()
	once          sync.Once
	done          chan struct{}
	gate          chan struct{}
}

func newStreamConn(st *yamux.Stream, local, remote net.Addr, gate chan struct{}, release func()) *streamConn {
	return &streamConn{Stream: st, local: local, remote: remote, gate: gate, release: release, done: make(chan struct{})}
}

func (c *streamConn) Write(p []byte) (int, error) {
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-c.done:
			return 0, net.ErrClosed
		}
	}
	return c.Stream.Write(p)
}

func (c *streamConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		if c.release != nil {
			c.release()
		}
	})
	return c.Stream.Close()
}

func (c *streamConn) LocalAddr() net.Addr  { return c.local }
func (c *streamConn) RemoteAddr() net.Addr { return c.remote }

type closeWriter interface{ CloseWrite() error }

// pipe copies both ways between a and b until both directions end, half-closing each destination when its source
// ends; an error, ctx ending or the grace after the first direction ended closes both. aToB and bToA count bytes.
func pipe(ctx context.Context, a, b net.Conn, aToB, bToA *atomic.Uint64) {
	closeBoth := func() { _ = a.Close(); _ = b.Close() }
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	var first sync.Once
	var grace *time.Timer
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn, count *atomic.Uint64) {
		defer wg.Done()
		n, err := io.Copy(dst, src)
		count.Add(uint64(n))
		if err != nil {
			closeBoth()
			return
		}
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		first.Do(func() { grace = time.AfterFunc(halfCloseGrace, closeBoth) })
	}
	wg.Add(2)
	go cp(b, a, aToB)
	go cp(a, b, bToA)
	wg.Wait()
	first.Do(func() {})
	if grace != nil {
		grace.Stop()
	}
}
