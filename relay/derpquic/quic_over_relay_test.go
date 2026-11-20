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
	// delayed carries writes through a fixed one-way delay (nil: none), in order, as a long path would.
	delayed chan delayedPacket
	delay   time.Duration
}

// pathJitter is 25 ms per direction (50 ms on the round trip) in every other half second: on a 500 ms path that is a 10% rise of the round trip, below HyStart's 1/8 but
// above upstream quic-go's 16 ms clamp.
func pathJitter(now time.Time) time.Duration {
	if now.UnixMilli()/500%2 == 1 {
		return 25 * time.Millisecond // per direction: 50 ms on the round trip
	}
	return 0
}

type delayedPacket struct {
	data []byte
	at   time.Time
}

type relayAddr struct{}

func (relayAddr) Network() string { return "relay" }
func (relayAddr) String() string  { return "relay-peer" }

func newRelayConn(c *Client, peer key.NodePublic, oneWayDelay time.Duration) *relayConn {
	r := &relayConn{c: c, peer: peer, in: make(chan []byte, 4096), done: make(chan struct{}), wake: make(chan struct{}, 1), delay: oneWayDelay}
	if oneWayDelay > 0 {
		r.delayed = make(chan delayedPacket, 8192)
		go func() {
			var last time.Time
			for {
				select {
				case p := <-r.delayed:
					// the delay steps up by pathJitter every other half second (a queue elsewhere on the path), never reordering
					if last.After(p.at) {
						p.at = last
					}
					last = p.at
					time.Sleep(time.Until(p.at))
					if r.c.Send(r.peer, p.data) != nil {
						return
					}
				case <-r.done:
					return
				}
			}
		}()
	}
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
	if r.delayed != nil {
		select {
		case r.delayed <- delayedPacket{append([]byte(nil), p...), time.Now().Add(r.delay + pathJitter(time.Now()))}:
			return len(p), nil
		default:
			return len(p), nil // a full delay line drops, as a full path buffer would
		}
	}
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
// duration and returns the goodput in Mbit/s; oneWayDelay is added to every packet of each peer (a path's round trip is twice it).
func quicThroughput(t *testing.T, mbit float64, oneWayDelay, duration time.Duration, w writes) (goodput float64, dropped, wire uint64) {
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
	ra, rb := newRelayConn(a, kb, oneWayDelay), newRelayConn(b, ka, oneWayDelay)
	defer ra.Close()
	defer rb.Close()
	s.TakeAccountDemand() // a window that starts with the flow
	received := new(atomic.Int64)
	goodput = quicBulk(t, ra, rb, relayAddr{}, duration, received, w)
	return goodput, s.Snapshot().Dropped, s.TakeAccountDemand()["quicaccount"].Offered
}

// writes is how a bulk sender writes: size bytes per Write, one every `every` (zero: as fast as the stream takes them).
type writes struct {
	size  int
	every time.Duration
}

var bulkWrites = writes{size: 32 << 10}

// quicBulk runs a QUIC bulk upload from ra's side to rb's over two packet conns for the given duration and returns the goodput in Mbit/s;
// received counts the payload bytes the receiver has read, so a caller can watch the flow while it runs.
func quicBulk(t *testing.T, ra, rb net.PacketConn, dial net.Addr, duration time.Duration, received *atomic.Int64, w writes) float64 {
	t.Helper()
	innerCert := certificate(t)
	server := &quic.Transport{Conn: rb}
	defer server.Close()
	listener, err := server.Listen(&tls.Config{Certificates: []tls.Certificate{innerCert}, NextProtos: []string{"bulk"}}, &quic.Config{MaxIdleTimeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
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
	chunk := make([]byte, w.size)
	go func() {
		for next := time.Now(); time.Now().Before(deadline); {
			if _, err := stream.Write(chunk); err != nil {
				return
			}
			if w.every > 0 { // paced: a write every w.every, catching up when the sleep ran long
				if next = next.Add(w.every); time.Until(next) > 0 {
					time.Sleep(time.Until(next))
				}
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
		goodput, dropped, _ := quicThroughput(t, mbit, 0, 8*time.Second, bulkWrites)
		t.Logf("account speed %4.1f Mbit/s: QUIC goodput %.2f Mbit/s, relay dropped %d datagrams", mbit, goodput, dropped)
		if goodput < 0.75*mbit || goodput > 1.05*mbit {
			t.Fatalf("a QUIC transfer through an account capped at %.1f Mbit/s got %.2f Mbit/s, want 75-105%% of it", mbit, goodput)
		}
	}
}

// A QUIC transfer over a path with a 500 ms round trip still gets most of the account speed. quic-go's HyStart used to leave slow start
// after 16 ms of delay jitter, at about 69 KB of congestion window, and then grew the window by one packet per round trip: 1.4 Mbit/s
// whatever the plan (third_party/quic-go/README.paperboat.md). With the patch a 5 Mbit/s account gets about 4.2 Mbit/s.
func TestQUICBulkOverALongPathGetsMostOfTheAccountSpeed(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 25 s")
	}
	goodput, dropped, _ := quicThroughput(t, 5, 250*time.Millisecond, 20*time.Second, bulkWrites)
	t.Logf("500 ms round trip, account speed 5 Mbit/s: QUIC goodput %.2f Mbit/s, relay dropped %d datagrams", goodput, dropped)
	if goodput < 3.5 || goodput > 5.25 {
		t.Fatalf("a QUIC transfer over a 500 ms path through an account capped at 5 Mbit/s got %.2f Mbit/s, want 3.5-5.25 (70-105%%)", goodput)
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
	delayMs, _ := strconv.Atoi(os.Getenv("RELAY_QUIC_DELAY_MS")) // one-way delay per peer; the round trip is twice it
	for _, field := range strings.Split(caps, ",") {
		mbit, err := strconv.ParseFloat(strings.TrimSpace(field), 64)
		if err != nil {
			t.Fatal(err)
		}
		goodput, dropped, _ := quicThroughput(t, mbit, time.Duration(delayMs)*time.Millisecond, time.Duration(seconds)*time.Second, bulkWrites)
		t.Logf("MEASURE cap %6.1f Mbit/s: QUIC goodput %7.2f Mbit/s (%.0f%% of the cap), relay dropped %d datagrams", mbit, goodput, 100*goodput/max(mbit, 1e-9), dropped)
	}
}

// shareLoop is the control plane's part in a test: every interval it reads what the account offered each relay and how much was held
// back, divides the plan speed among them with relayshare.Shares (as the control plane does for each observation) and sets every
// relay's share. The production interval is 3 s (nodelifecycle.Interval).
func shareLoop(t *testing.T, servers []*Server, account string, plan float64, interval time.Duration) (stop func()) {
	t.Helper()
	step := func(dt float64) {
		demand := map[string]relayshare.Demand{}
		for i, s := range servers {
			d := s.TakeAccountDemand()[account]
			demand[strconv.Itoa(i)] = relayshare.Demand{Offered: float64(d.Offered) / dt, Held: float64(d.Held) / dt}
		}
		shares := relayshare.Shares(plan, demand)
		for i, s := range servers {
			if err := s.SetAccountSpeeds(map[string]float64{account: shares[strconv.Itoa(i)]}); err != nil {
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
		pairs[i] = [2]*relayConn{newRelayConn(a, kb, 0), newRelayConn(b, ka, 0)}
		t.Cleanup(func() { pairs[i][0].Close(); pairs[i][1].Close() })
	}
	return servers, pairs, shareLoop(t, servers, "shareaccount", plan, 3*time.Second)
}

// flow runs a bulk upload over a pair in the background and returns its received-byte counter and a wait for its end.
func flow(t *testing.T, pair [2]*relayConn, duration time.Duration) (*atomic.Int64, func()) {
	received, done := new(atomic.Int64), make(chan struct{})
	go func() { defer close(done); quicBulk(t, pair[0], pair[1], relayAddr{}, duration, received, bulkWrites) }()
	return received, func() { <-done }
}

// mbitOver is the goodput of a flow between two readings of its counter.
func mbitOver(from, to int64, window time.Duration) float64 {
	return float64(to-from) * 8 / 1e6 / window.Seconds()
}

// An account's speed is one budget across its relays, divided by demand, with the production 3 s observation: one active flow on one of
// two relays runs at the plan speed as goodput (at least 95%: the idle relay holds only its start allowance, and the budget counts the
// wire overhead on top of the plan speed), a second flow on the other relay is given its half within two observations, and the two
// together stay within the plan speed.
func TestAccountSpeedIsSharedAcrossRelaysByDemand(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 45 s")
	}
	const plan = 10.0

	servers, pairs, stop := twoRelays(t, plan)
	first, wait := flow(t, pairs[0], 40*time.Second)
	time.Sleep(8 * time.Second) // past the first observation (3 s) and the transport's ramp
	from, steadyFrom := first.Load(), time.Now()
	time.Sleep(6 * time.Second)
	alone := mbitOver(from, first.Load(), time.Since(steadyFrom))
	t.Logf("one active flow: %.2f Mbit/s of a %.0f Mbit/s plan (relay 1 held at %.2f, the idle relay 2 at %.3f)", alone, plan, servers[0].AccountMbit("shareaccount"), servers[1].AccountMbit("shareaccount"))
	if alone < 0.95*plan {
		t.Errorf("one active flow got %.2f Mbit/s of a %.0f Mbit/s plan, want at least 95%%", alone, plan)
	}

	// A second flow starts on the other relay: how long until it runs at 80% of its fair half?
	second, waitSecond := flow(t, pairs[1], 22*time.Second)
	secondAt := time.Now()
	var ramp time.Duration
	last, lastAt := second.Load(), secondAt
	var firstFrom, secondFrom, firstTo, secondTo int64
	var windowFrom time.Time
	for time.Since(secondAt) < 21*time.Second {
		time.Sleep(250 * time.Millisecond)
		now, at := second.Load(), time.Now()
		if windowFrom.IsZero() && at.Sub(secondAt) >= 13*time.Second {
			firstFrom, secondFrom, windowFrom = first.Load(), now, at
		}
		if rate := mbitOver(last, now, at.Sub(lastAt)); ramp == 0 && rate >= 0.8*plan/2 && at.Sub(secondAt) > time.Second {
			ramp = at.Sub(secondAt)
		}
		last, lastAt = now, at
	}
	firstTo, secondTo = first.Load(), second.Load()
	both, window := mbitOver(firstFrom, firstTo, time.Since(windowFrom))+mbitOver(secondFrom, secondTo, time.Since(windowFrom)), time.Since(windowFrom)
	t.Logf("both flows over the last %.0f s: %.2f + %.2f = %.2f Mbit/s of %.0f", window.Seconds(), mbitOver(firstFrom, firstTo, window), mbitOver(secondFrom, secondTo, window), both, plan)
	if both > 1.05*plan || both < 0.7*plan {
		t.Errorf("two flows together got %.2f Mbit/s of a %.0f Mbit/s plan, want at most 105%% (and at least 70%%)", both, plan)
	}
	waitSecond()
	wait()
	stop()
	t.Logf("a second flow on relay 2 ramped to 80%% of its half (%.1f Mbit/s) after %.1f s", 0.8*plan/2, ramp.Seconds())
	if ramp == 0 || ramp > 8*time.Second {
		t.Errorf("the second flow took %v to reach 80%% of its half, want at most 8 s (two observations of 3 s and the transport's ramp)", ramp)
	}
}

// Terminal-like traffic: a flow of small writes (about 200 B) goes through the relay at the plan speed too. Written as fast as the stream
// takes them the sender packs them into full packets and the flow is a bulk one; paced (a write every 250 us, 6.4 Mbit/s offered) the
// packets are small and the wire overhead, which the budget grosses up for, is larger.
func TestQUICSmallWritesThroughRelayGetsTheirSpeed(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 20 s")
	}
	const plan = 10.0
	fast, _, fastWire := quicThroughput(t, plan, 0, 8*time.Second, writes{size: 200})
	t.Logf("200 B writes, as fast as possible: goodput %.2f Mbit/s of a %.0f Mbit/s plan; the relay counted %.2f Mbit/s on the wire", fast, plan, float64(fastWire)*8/1e6/8)
	if fast < 0.8*plan {
		t.Errorf("200 B writes got %.2f Mbit/s of a %.0f Mbit/s plan, want at least 80%%", fast, plan)
	}
	paced, _, pacedWire := quicThroughput(t, plan, 0, 8*time.Second, writes{size: 200, every: 250 * time.Microsecond})
	t.Logf("200 B writes every 250 us (6.4 Mbit/s offered): goodput %.2f Mbit/s; the relay counted %.2f Mbit/s on the wire (%.0f%% overhead)", paced, float64(pacedWire)*8/1e6/8, 100*(float64(pacedWire)*8/1e6/8/paced-1))
	if paced < 0.9*6.4 {
		t.Errorf("paced 200 B writes got %.2f Mbit/s of 6.4 offered, want at least 90%%", paced)
	}
}
