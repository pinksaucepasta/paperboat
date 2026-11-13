//go:build darwin || linux

package tunnel

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

// X-24 on a real client: `pb connect` reads the daemon's terminal through the local stream (ServeLocalPeerConn) and its own queue, and the
// console paints at a few hundred KB/s (a Windows ConPTY, measured 160 KB/s: 5-8 MB kept coming 30-50 s after Ctrl-C).
// Whatever the local stream holds below the Ctrl-C cut is painted after it, so the bound is what the stream may hold.
func TestCtrlCUnderAFloodStopsThePaintThroughTheLocalStream(t *testing.T) {
	host, authorizer, sessions, target := terminalReplayHost(t)
	wire := newExecCanaryWire()
	wire.serverToClient = make(chan execMessage, 65536)
	remote := attachReplayTerminal(t, host, authorizer, &target, wire)
	socket := filepath.Join(t.TempDir(), "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		local, err := listener.Accept()
		if err == nil {
			_ = ServeLocalPeerConn(context.Background(), local, remote)
		}
	}()
	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	local, err := NewLocalPeerConn(client, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// What pb connect reads from: its own queue sits between the stream and the screen (Connect.TerminalOutputQueueChunks).
	connection := NewObservedReconnectingConn(context.Background(), local, 0, time.Second,
		func(context.Context) (Conn, error) { return nil, ErrTransportLost }, nil, nil, TelemetryContext{},
		WithReconnectingOutput(config.DefaultTerminalOutputQueueChunks, 0))
	defer connection.Close()
	var painted atomic.Int64
	marker := make(chan time.Time, 1)
	go func() {
		var tail []byte
		buffer := make([]byte, 4096)
		for {
			n, err := connection.Read(buffer)
			painted.Add(int64(n))
			if tail = append(tail, buffer[:n]...); bytes.Contains(tail, []byte("PB_STOPPED")) {
				marker <- time.Now()
				return
			}
			tail = tail[max(0, len(tail)-64):]
			if err != nil {
				return
			}
			time.Sleep(time.Duration(n) * time.Second / (160 << 10)) // a slow console
		}
	}()
	if _, err := connection.Write([]byte("yes\n")); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if snapshot, err := sessions.Snapshot(target.SessionID); err == nil && snapshot.LatestSequence >= 12<<20 {
			break
		}
	}
	keyAt, before := time.Now(), painted.Load()
	if _, err := connection.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte("printf 'PB_%s\\n' STOPPED\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case at := <-marker:
		t.Logf("the screen was quiet %v after the key, %d bytes painted after it", at.Sub(keyAt), painted.Load()-before)
		if at.Sub(keyAt) > time.Second {
			t.Fatalf("the screen took %v to stop after Ctrl-C, want at most 1s", at.Sub(keyAt))
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("no sign of the shell 60 s after Ctrl-C, %d bytes painted after it", painted.Load()-before)
	}
}
