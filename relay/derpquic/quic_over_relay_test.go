package derpquic

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/relayshare"
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
	return quicBulk(t, ra, rb, relayAddr{}, duration), s.Snapshot().Dropped
}

// quicBulk runs a QUIC bulk upload from ra's side to rb's over two packet conns for the given duration and returns the goodput in Mbit/s.
func quicBulk(t *testing.T, ra, rb net.PacketConn, dial net.Addr, duration time.Duration) float64 {
	t.Helper()
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
	connection, err := client.Dial(ctx, dial, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"bulk"}}, &quic.Config{MaxIdleTimeout: 20 * time.Second})
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
	return float64(received.Load()) * 8 / 1e6 / elapsed
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

// Measurement for a live A/B (not part of the suite): RELAY_QUIC_MEASURE=1 go test -run QUICBulkMeasure -v, optionally with network delay on
// the loopback (tc qdisc add dev lo root netem delay 62ms gives a 250 ms round trip) to stand in for a far relay.
// RELAY_QUIC_CAPS is a comma list of account speeds in Mbit/s (0 lifts the cap), RELAY_QUIC_SECONDS the length of each run.
func TestQUICBulkMeasure(t *testing.T) {
	if os.Getenv("RELAY_QUIC_MEASURE") == "" {
		t.Skip("set RELAY_QUIC_MEASURE=1")
	}
	caps, seconds := os.Getenv("RELAY_QUIC_CAPS"), 8
	if caps == "" {
		caps = "100,10,5,1"
	}
	if v, err := strconv.Atoi(os.Getenv("RELAY_QUIC_SECONDS")); err == nil && v > 0 {
		seconds = v
	}
	for _, field := range strings.Split(caps, ",") {
		mbit, err := strconv.ParseFloat(strings.TrimSpace(field), 64)
		if err != nil {
			t.Fatal(err)
		}
		goodput, dropped := quicThroughput(t, mbit, time.Duration(seconds)*time.Second)
		t.Logf("MEASURE cap %6.1f Mbit/s: QUIC goodput %7.2f Mbit/s (%.0f%% of the cap), relay dropped %d datagrams", mbit, goodput, 100*goodput/max(mbit, 1e-9), dropped)
	}
}

// shareLoop is the control plane's part in a test: every interval it reads what the account offered each relay, divides the plan speed
// among them with relayshare.Share (as the control plane does for each observation) and sets every relay's share.
func shareLoop(t *testing.T, servers []*Server, account string, plan float64, interval time.Duration) (stop func()) {
	t.Helper()
	step := func(dt float64) {
		demand := map[string]float64{}
		for i, s := range servers {
			demand[strconv.Itoa(i)] = float64(s.TakeAccountDemand()[account]) / dt
		}
		for i, s := range servers {
			if err := s.SetAccountSpeeds(map[string]float64{account: relayshare.Share(plan, demand[strconv.Itoa(i)], demand)}); err != nil {
				t.Error(err)
			}
		}
	}
	step(1) // no demand yet: an even split before the first flow starts
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		last := time.Now()
		for ticker := time.NewTicker(interval); ; {
			select {
			case now := <-ticker.C:
				step(now.Sub(last).Seconds())
				last = now
			case <-done:
				ticker.Stop()
				return
			}
		}
	}()
	return func() { close(done); <-finished }
}

// twoRelays starts two relays holding one account at the plan speed, with the share loop dividing it, and a connected peer pair of the
// account on each; it returns the servers and the pairs' packet conns.
func twoRelays(t *testing.T, plan float64) (servers []*Server, pairs [2][2]*relayConn, stop func()) {
	t.Helper()
	f := newFixture(t)
	for i := range pairs {
		s, address, _ := f.start("127.0.0.1:0")
		if err := s.SetAccountSpeed(plan); err != nil {
			t.Fatal(err)
		}
		servers = append(servers, s)
		ka, kb := key.NewNode().Public(), key.NewNode().Public()
		ca, cb := certificate(t), certificate(t)
		ga, gb := f.grant(ka, ca, "share-a"), f.grant(kb, cb, "share-b")
		ga.AccountID, gb.AccountID = "shareaccount", "shareaccount"
		pairScopes(&ga, &gb)
		a, b := f.client(address, ca, ga), f.client(address, cb, gb)
		connect(t, a)
		connect(t, b)
		pairs[i] = [2]*relayConn{newRelayConn(a, kb), newRelayConn(b, ka)}
		t.Cleanup(func() { pairs[i][0].Close(); pairs[i][1].Close() })
	}
	return servers, pairs, shareLoop(t, servers, "shareaccount", plan, 500*time.Millisecond)
}

// An account's speed is one budget across its relays, divided by demand: a single flow on one of two relays gets at least 90% of the plan
// speed at the relay (the idle relay holds only its floor) and nearly all of it as QUIC goodput, and two flows on two relays together
// stay within the plan speed.
func TestAccountSpeedIsSharedAcrossRelaysByDemand(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 30 s")
	}
	const plan = 10.0
	duration := 12 * time.Second

	servers, pairs, stop := twoRelays(t, plan)
	var single float64
	done := make(chan struct{})
	go func() { defer close(done); single = quicBulk(t, pairs[0][0], pairs[0][1], relayAddr{}, duration) }()
	time.Sleep(duration / 2)
	atFlow, atIdle := servers[0].AccountMbit("shareaccount"), servers[1].AccountMbit("shareaccount")
	<-done
	stop()
	t.Logf("one flow on relay 1: QUIC goodput %.2f Mbit/s; relay 1 held at %.2f, the idle relay 2 at %.2f of %.0f", single, atFlow, atIdle, plan)
	if atFlow < 0.9*plan || atFlow+atIdle > plan*1.001 || single < 0.8*plan {
		t.Fatalf("one flow got relay speed %.2f (+%.2f idle) and %.2f Mbit/s goodput of a %.0f Mbit/s account: want at least 90%% at the relay, the shares within the plan, at least 80%% as goodput", atFlow, atIdle, single, plan)
	}

	_, pairs, stop = twoRelays(t, plan)
	var both [2]float64
	done = make(chan struct{})
	go func() {
		defer close(done)
		both[1] = quicBulk(t, pairs[1][0], pairs[1][1], relayAddr{}, duration)
	}()
	both[0] = quicBulk(t, pairs[0][0], pairs[0][1], relayAddr{}, duration)
	<-done
	stop()
	t.Logf("two flows on two relays: %.2f + %.2f = %.2f Mbit/s of %.0f", both[0], both[1], both[0]+both[1], plan)
	if sum := both[0] + both[1]; sum > 1.05*plan || sum < 0.6*plan {
		t.Fatalf("two flows together got %.2f Mbit/s of a %.0f Mbit/s account: want at most 105%% and, as a sanity floor, at least 60%%", sum, plan)
	}
}
