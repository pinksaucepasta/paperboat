package derpquic

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

// BenchmarkRelayForward measures lossless relay forwarding of full-size
// WireGuard packets between two carriers: the sender keeps a bounded window in
// flight so the measured rate is the relay's, not queue overflow.
func BenchmarkRelayForward(b *testing.B) {
	f := newFixture(b)
	_, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(b), certificate(b)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	pairScopes(&ga, &gb)
	sender, receiver := f.client(address, ca, ga), f.client(address, cb, gb)
	connect(b, sender)
	connect(b, receiver)
	var received, lost atomic.Int64
	go func() {
		for {
			m, _, err := receiver.RecvDetail()
			if err != nil {
				return
			}
			if _, ok := m.(derp.ReceivedPacket); ok {
				received.Add(1)
			}
		}
	}()
	const window, size = queueDepth / 2, 1312
	data := make([]byte, size)
	data[0] = 4
	b.SetBytes(size)
	b.ResetTimer()
	for sent := int64(0); sent < int64(b.N); {
		deadline := time.Now().Add(100 * time.Millisecond)
		for sent-received.Load()-lost.Load() >= window {
			if time.Now().After(deadline) {
				// A lost datagram must not stall the window forever.
				lost.Add(sent - received.Load() - lost.Load())
			}
			runtime.Gosched()
		}
		if err := sender.Send(kb, data); err != nil {
			b.Fatal(err)
		}
		sent++
	}
	b.StopTimer()
	b.ReportMetric(float64(lost.Load())/float64(b.N), "lost/op")
}
