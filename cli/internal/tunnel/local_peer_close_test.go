package tunnel

import (
	"net"
	"sync"
	"testing"
	"time"
)

type observedLocalWriteConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *observedLocalWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestLocalPeerCloseBoundsBlockedWriterAndFullOutputQueue(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	defer client.Close()
	observed := &observedLocalWriteConn{Conn: client, started: make(chan struct{})}
	connection, err := NewLocalPeerConn(observed, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		writer := &localPeerWriter{writer: peer}
		for i := 0; i < 20; i++ {
			if writer.write(localPeerTerminalData, make([]byte, 9)) != nil {
				return
			}
		}
	}()
	inner := connection.(*localBasicPeerConn).inner
	deadline := time.Now().Add(time.Second)
	for len(inner.data) != cap(inner.data) {
		if time.Now().After(deadline) {
			t.Fatal("output queue did not fill")
		}
		time.Sleep(time.Millisecond)
	}
	written := make(chan struct{})
	go func() { defer close(written); _, _ = connection.Write([]byte("input")) }()
	<-observed.started
	closed := make(chan struct{})
	go func() { defer close(closed); _ = connection.Close() }()
	select {
	case <-closed:
	case <-time.After(4 * time.Second):
		_ = client.Close()
		t.Fatal("close exceeded its deadline behind a blocked writer")
	}
	for _, done := range []<-chan struct{}{written, produced, connection.(*localBasicPeerConn).inner.done} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("close left a stream goroutine blocked")
		}
	}
}
