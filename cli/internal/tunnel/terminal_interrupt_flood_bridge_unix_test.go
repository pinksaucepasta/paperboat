//go:build darwin || linux

package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
)

// X-24 on a real client: `pb connect` reads the daemon's terminal through the local stream (ServeLocalPeerConn) and its own queue, and the
// console paints at a few hundred KB/s (a Windows ConPTY, measured 160 KB/s: 5-8 MB kept coming 30-50 s after Ctrl-C).
// Whatever the local stream holds below the Ctrl-C cut is painted after it, so the bound is what the stream may hold.
func TestCtrlCUnderAFloodStopsThePaintThroughTheLocalStream(t *testing.T) {
	host, authorizer, sessions, target := terminalReplayHost(t)
	wire := newExecCanaryWire()
	wire.serverToClient = make(chan execMessage, 65536)
	remote := attachReplayTerminal(t, host, authorizer, &target, wire)
	socket := filepath.Join(shorttemp.Dir(t), "s")
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

// N6: yes floods, a paste is stuck behind it, Ctrl-C: the window must keep showing output. The daemon's whole path is here
// (cursor terminal, local stream with its committed cursor, the CLI's queue), because the cursor is what ended the output.
func TestCtrlCWithAPasteInFlightKeepsTheWindowAlive(t *testing.T) {
	host, authorizer, _, target := terminalReplayHost(t)
	bridge := &LocalPeerCursorBridge{}
	sequence := &atomic.Int64{}
	target.SequenceSink = func(value int) { sequence.Store(int64(value)) }
	remote := attachReplayTerminal(t, host, authorizer, &target, newExecCanaryWire())
	client, daemon := net.Pipe()
	go func() {
		_ = ServeLocalPeerTerminalConn(context.Background(), daemon, newCursorTerminal(context.Background(), remote, sequence, bridge.RecordSequence), bridge)
	}()
	local, err := NewLocalPeerConn(client, func(int) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	connection := NewObservedReconnectingConn(context.Background(), local, 0, time.Second,
		func(context.Context) (Conn, error) { return nil, ErrTransportLost }, nil, nil, TelemetryContext{},
		WithReconnectingOutput(config.DefaultTerminalOutputQueueChunks, 0))
	defer connection.Close()
	markers := make(chan string, 64)
	var painted atomic.Int64
	go func() {
		var tail []byte
		buffer := make([]byte, 4096)
		for {
			n, err := connection.Read(buffer)
			painted.Add(int64(n))
			tail = append(tail, buffer[:n]...)
			if at := bytes.Index(tail, []byte("PB_STOPPED")); at >= 0 && bytes.IndexByte(tail[at:], '\r') >= 0 {
				end := at + bytes.IndexByte(tail[at:], '\r')
				markers <- string(tail[at:end])
				tail = tail[end:]
			}
			tail = tail[max(0, len(tail)-256):]
			if err != nil {
				return
			}
		}
	}()
	write := func(data []byte) {
		t.Helper()
		if _, err := connection.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	paste := bytes.Repeat(append(bytes.Repeat([]byte("x"), 1023), '\n'), 256)
	for round := range 20 {
		before := painted.Load()
		write([]byte("yes\n"))
		for deadline := time.Now().Add(10 * time.Second); painted.Load() < before+2<<20 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		}
		write(paste)
		write([]byte{0x03})
		time.Sleep(300 * time.Millisecond)
		write(fmt.Appendf(nil, "printf 'PB_%%s_%d\\r\\n' STOPPED\n", round))
		select {
		case got := <-markers:
			if want := fmt.Sprintf("PB_STOPPED_%d", round); got != want {
				t.Fatalf("round %d answered %q", round, got)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("round %d: no output from the shell 15 s after Ctrl-C, cursor %d", round, sequence.Load())
		}
	}
}
