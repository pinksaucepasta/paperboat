package derpquic

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"tailscale.com/derp"
	"tailscale.com/types/key"
)

// relayConn is a net.PacketConn over a relay client: every datagram goes to one peer through the relay, as pb's native transport
// carries its own QUIC.
type relayConn struct {
	c        *Client
	peer     key.NodePublic
	in       chan []byte
	done     chan struct{}
	wake     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	deadline time.Time
}

type relayAddr struct{}

func (relayAddr) Network() string { return "relay" }
func (relayAddr) String() string  { return "relay-peer" }

func newRelayConn(c *Client, peer key.NodePublic) *relayConn {
	r := &relayConn{c: c, peer: peer, in: make(chan []byte, 4096), done: make(chan struct{}), wake: make(chan struct{}, 1)}
	go func() {
		for {
			message, _, err := c.RecvDetail()
			if err != nil {
				r.Close()
				return
			}
			if packet, ok := message.(derp.ReceivedPacket); ok && packet.Source == peer {
				select {
				case r.in <- packet.Data:
				case <-r.done:
					return
				}
			}
		}
	}()
	return r
}

func (r *relayConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		r.mu.Lock()
		deadline := r.deadline
		r.mu.Unlock()
		var expired <-chan time.Time
		if !deadline.IsZero() {
			wait := time.Until(deadline)
			if wait <= 0 {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer := time.NewTimer(wait)
			defer timer.Stop()
			expired = timer.C
		}
		select {
		case data := <-r.in:
			return copy(p, data), relayAddr{}, nil
		case <-expired:
			return 0, nil, os.ErrDeadlineExceeded
		case <-r.wake:
		case <-r.done:
			return 0, nil, net.ErrClosed
		}
	}
}

func (r *relayConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	if err := r.c.Send(r.peer, append([]byte(nil), p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *relayConn) Close() error                  { r.once.Do(func() { close(r.done) }); return nil }
func (r *relayConn) LocalAddr() net.Addr           { return relayAddr{} }
func (r *relayConn) SetDeadline(t time.Time) error { return r.SetReadDeadline(t) }
func (r *relayConn) SetReadDeadline(t time.Time) error {
	r.mu.Lock()
	r.deadline = t
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return nil
}
func (r *relayConn) SetWriteDeadline(time.Time) error { return nil }

// quicThroughput runs a QUIC bulk upload from peer a to peer b through a relay whose account speed is mbit (0 lifts the cap) for the given
// duration and returns the goodput in Mbit/s.
func quicThroughput(t *testing.T, mbit float64, duration time.Duration) (goodput float64, dropped uint64) {
	t.Helper()
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	if err := s.SetAccountSpeed(mbit); err != nil {
		t.Fatal(err)
	}
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "quic-a"), f.grant(kb, cb, "quic-b")
	ga.AccountID, gb.AccountID = "quicaccount", "quicaccount"
	pairScopes(&ga, &gb)
	a, b := f.client(address, ca, ga), f.client(address, cb, gb)
	connect(t, a)
	connect(t, b)
	ra, rb := newRelayConn(a, kb), newRelayConn(b, ka)
	defer ra.Close()
	defer rb.Close()
	innerCert := certificate(t)
	server := &quic.Transport{Conn: rb}
	defer server.Close()
	listener, err := server.Listen(&tls.Config{Certificates: []tls.Certificate{innerCert}, NextProtos: []string{"bulk"}}, &quic.Config{MaxIdleTimeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var received atomic.Int64
	go func() {
		connection, err := listener.Accept(context.Background())
		if err != nil {
			return
		}
		stream, err := connection.AcceptStream(context.Background())
		if err != nil {
			return
		}
		buffer := make([]byte, 64<<10)
		for {
			n, err := stream.Read(buffer)
			received.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	client := &quic.Transport{Conn: ra}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), duration+20*time.Second)
	defer cancel()
	connection, err := client.Dial(ctx, relayAddr{}, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"bulk"}}, &quic.Config{MaxIdleTimeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("inner QUIC handshake through the relay: %v", err)
	}
	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	deadline := started.Add(duration)
	chunk := make([]byte, 32<<10)
	go func() {
		for time.Now().Before(deadline) {
			if _, err := stream.Write(chunk); err != nil {
				return
			}
		}
		_ = stream.Close()
	}()
	time.Sleep(duration)
	elapsed := time.Since(started).Seconds()
	_ = io.Discard
	return float64(received.Load()) * 8 / 1e6 / elapsed, s.Snapshot().Dropped
}

// A QUIC transfer through the relay gets most of its account's speed. The account budget used to charge every datagram at least a full
// fragment, so a 60-byte ACK cost as much as a data packet and a 1252-byte inner packet (two fragments) cost twice its size: a
// transfer got 47% of a 10 Mbit/s account (4.7 Mbit/s), 52% of 100 and 26% of 1, which is the crawl users saw on relayed transfers.
func TestQUICBulkThroughRelayGetsMostOfTheAccountSpeed(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 20 s")
	}
	for _, mbit := range []float64{10, 1} {
		goodput, dropped := quicThroughput(t, mbit, 8*time.Second)
		t.Logf("account speed %4.1f Mbit/s: QUIC goodput %.2f Mbit/s, relay dropped %d datagrams", mbit, goodput, dropped)
		if goodput < 0.75*mbit || goodput > 1.05*mbit {
			t.Fatalf("a QUIC transfer through an account capped at %.1f Mbit/s got %.2f Mbit/s, want 75-105%% of it", mbit, goodput)
		}
	}
}

// The account budget charges bytes (at least accountMinimumCost); the carrier budget keeps charging a full fragment for any datagram, so
// a flood of tiny frames from one peer is still bounded.
func TestAccountBudgetChargesBytesWhereTheCarrierBudgetChargesAFragment(t *testing.T) {
	account, carrier := newAccountBudget(1<<20), newBudget(carrierBytesPerSecond)
	if got := account.charge(60); got != accountMinimumCost {
		t.Fatalf("account charge for a 60-byte ACK = %v, want %d", got, accountMinimumCost)
	}
	if got := account.charge(1067); got != 1067 {
		t.Fatalf("account charge for a full datagram = %v, want 1067", got)
	}
	if got := carrier.charge(60); got != budgetMinimumCost {
		t.Fatalf("carrier charge for a 60-byte datagram = %v, want %d", got, budgetMinimumCost)
	}
}
