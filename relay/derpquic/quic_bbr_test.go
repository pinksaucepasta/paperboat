package derpquic

import (
	"context"
	"crypto/tls"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// linkConn is a net.PacketConn whose writes cross a simulated bottleneck: a fixed one-way delay, a rate in bytes per
// second behind a drop-tail queue of at most maxQueue, and independent random loss.
type linkConn struct {
	net.PacketConn
	delay    time.Duration
	rate     float64
	maxQueue time.Duration
	loss     float64
	rng      *rand.Rand
	in       chan linkPacket
	mu       sync.Mutex
	nextFree time.Time
}

type linkPacket struct {
	data []byte
	to   net.Addr
	at   time.Time
}

func newLinkConn(c net.PacketConn, delay time.Duration, rate float64, maxQueue time.Duration, loss float64, seed int64) *linkConn {
	l := &linkConn{PacketConn: c, delay: delay, rate: rate, maxQueue: maxQueue, loss: loss, rng: rand.New(rand.NewSource(seed)), in: make(chan linkPacket, 1<<16)}
	go func() {
		for p := range l.in {
			time.Sleep(time.Until(p.at))
			_, _ = l.PacketConn.WriteTo(p.data, p.to)
		}
	}()
	return l
}

func (l *linkConn) WriteTo(p []byte, to net.Addr) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	begin := l.nextFree
	if begin.Before(now) {
		begin = now
	}
	if begin.Sub(now) > l.maxQueue || l.rng.Float64() < l.loss {
		return len(p), nil
	}
	l.nextFree = begin.Add(time.Duration(float64(len(p)) / l.rate * 1e9))
	select {
	case l.in <- linkPacket{append([]byte(nil), p...), to, l.nextFree.Add(l.delay)}:
	default:
	}
	return len(p), nil
}

func (l *linkConn) Close() error {
	close(l.in)
	return l.PacketConn.Close()
}

// lossyTransfer sends for d over 50 ms and 0.5% random loss on a 50 Mbit/s link and returns the goodput in Mbit/s.
func lossyTransfer(t *testing.T, control quic.CongestionControl, d time.Duration) float64 {
	t.Helper()
	listen := func() net.PacketConn {
		p, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	rawServer, rawClient := listen(), listen()
	const rate = 50e6 / 8
	server := &quic.Transport{Conn: newLinkConn(rawServer, 25*time.Millisecond, rate, 100*time.Millisecond, 0, 2)}
	client := &quic.Transport{Conn: newLinkConn(rawClient, 25*time.Millisecond, rate, 100*time.Millisecond, 0.005, 1)}
	defer server.Close()
	defer client.Close()
	config := &quic.Config{CongestionControl: control, MaxIdleTimeout: 10 * time.Second}
	l, err := server.Listen(&tls.Config{Certificates: []tls.Certificate{certificate(t)}, NextProtos: []string{"bulk"}}, config)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), d+20*time.Second)
	defer cancel()
	var received atomic.Int64
	go func() {
		conn, err := l.Accept(ctx)
		if err != nil {
			return
		}
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		buf := make([]byte, 64<<10)
		for {
			n, err := stream.Read(buf)
			received.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	conn, err := client.Dial(ctx, rawServer.LocalAddr(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"bulk"}}, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = stream.SetWriteDeadline(start.Add(d))
	chunk := make([]byte, 64<<10)
	for {
		if _, err := stream.Write(chunk); err != nil {
			break
		}
	}
	return float64(received.Load()) * 8 / 1e6 / time.Since(start).Seconds()
}

// The benchmark's 50 ms / 0.5% cell, in process: NewReno sits at its Mathis ceiling (about 3 Mbit/s), BBR keeps the link.
func TestBBRBeatsRenoOnLossyLongPath(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 16 s")
	}
	reno := lossyTransfer(t, quic.CongestionControlReno, 8*time.Second)
	bbr := lossyTransfer(t, quic.CongestionControlBBR, 8*time.Second)
	t.Logf("reno %.1f Mbit/s, bbr %.1f Mbit/s", reno, bbr)
	if bbr < 3*reno || bbr < 15 {
		t.Fatalf("BBR %.1f Mbit/s must beat Reno %.1f Mbit/s by 3x and reach 15 Mbit/s", bbr, reno)
	}
}
