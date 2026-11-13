//go:build darwin || linux

package tunnel

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"
)

// X-24: Ctrl-C under an output flood has to stop the screen within a second, however slowly the client paints. The wire holds
// 65536 messages, which compressed `yes` output fills with hundreds of MB of decoded text, like the transport windows of a real
// link. Before the host paced the PTY by what the client acknowledged, the output queued ahead of the host's answer overflowed
// the Ctrl-C hold (helperHoldMaxBytes), was released, and was painted for as long as the painter needed (measured 18 MB, 20 s).
func TestCtrlCUnderAFloodStopsThePaintWithinOneSecond(t *testing.T) {
	host, authorizer, sessions, target := terminalReplayHost(t)
	wire := newExecCanaryWire()
	wire.serverToClient = make(chan execMessage, 65536)
	connection := attachReplayTerminal(t, host, authorizer, &target, wire)
	var painted atomic.Int64
	var paintBytesPerSecond atomic.Int64 // 64 KiB/s while the flood builds up, 1 MiB/s from the key on: a slow terminal
	paintBytesPerSecond.Store(64 << 10)
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
			time.Sleep(time.Duration(n) * time.Second / time.Duration(paintBytesPerSecond.Load())) // the slow painter
		}
	}()
	if _, err := connection.Write([]byte("yes\n")); err != nil {
		t.Fatal(err)
	}
	// The program prints until the host has published 12 MiB (more than the hold takes) or 5 s have passed: a host that paces
	// the PTY by what the client takes stays at about a window and waits out the 5 s.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if snapshot, err := sessions.Snapshot(target.SessionID); err == nil && snapshot.LatestSequence >= 12<<20 {
			break
		}
	}
	published := int64(0)
	if snapshot, err := sessions.Snapshot(target.SessionID); err == nil {
		published = int64(snapshot.LatestSequence)
	}
	keyAt := time.Now()
	if _, err := connection.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte("printf 'PB_%s\\n' STOPPED\n")); err != nil {
		t.Fatal(err)
	}
	paintBytesPerSecond.Store(1 << 20)
	select {
	case at := <-marker:
		t.Logf("the host had published %d bytes at the key; the screen was quiet %v after it, %d bytes painted", published, at.Sub(keyAt), painted.Load())
		if at.Sub(keyAt) > time.Second {
			t.Fatalf("the screen took %v to stop after Ctrl-C, want at most 1s", at.Sub(keyAt))
		}
	case <-time.After(40 * time.Second):
		t.Fatalf("no sign of the shell 40 s after Ctrl-C; the host had published %d bytes at the key, %d painted", published, painted.Load())
	}
}
