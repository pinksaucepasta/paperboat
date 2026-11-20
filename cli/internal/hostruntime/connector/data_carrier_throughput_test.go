package connector

import (
	"context"
	"io"
	"net"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	sharedprotocol "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/quicdial"
	"github.com/quic-go/quic-go"
)

const (
	throughputBytes  = 200 << 20
	throughputBudget = 150.0 // Mbit/s, PR-06: one stream at 50 ms RTT, on every OS the connector runs on
)

// delayedUDPRelay relays UDP between one client and target and delays the client's packets by delay (replies pass at
// once), so the sender sees an RTT of delay, as netem on the origin's egress gives it. It returns the address to dial.
func delayedUDPRelay(t *testing.T, target *net.UDPAddr, delay time.Duration) string {
	t.Helper()
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	back, err := net.DialUDP("udp", nil, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = front.Close(); _ = back.Close() })
	// A burst of a 50 ms flight (~2 MB) must fit: Windows' default UDP receive buffer is 64 KB, and the relay's drops
	// then show up as the sender's loss (3 connections, 49-176 lost packets, 100-140 Mbit/s instead of 300).
	for _, conn := range []*net.UDPConn{front, back} {
		_ = conn.SetReadBuffer(8 << 20)
		_ = conn.SetWriteBuffer(8 << 20)
	}
	type delayed struct {
		packet []byte
		due    time.Time
	}
	queue := make(chan delayed, 1<<16)
	var client atomic.Pointer[net.UDPAddr]
	go func() {
		buffer := make([]byte, 64<<10)
		for {
			n, from, err := front.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			client.Store(from)
			queue <- delayed{append([]byte(nil), buffer[:n]...), time.Now().Add(delay)}
		}
	}()
	go func() {
		for item := range queue {
			time.Sleep(time.Until(item.due))
			_, _ = back.Write(item.packet)
		}
	}()
	go func() {
		buffer := make([]byte, 64<<10)
		for {
			n, err := back.Read(buffer)
			if err != nil {
				return
			}
			if to := client.Load(); to != nil {
				_, _ = front.WriteToUDP(buffer[:n], to)
			}
		}
	}()
	return front.LocalAddr().String()
}

// One QUIC stream, sent the way the daemon dials (quicdial.Dial with defaultQUICConfig on the OS socket), into the
// edge's receive windows (256 KiB initial, 4 MiB maximum, as edge/internal/datacarrier endpointQUICConfig) must hold the
// budget at 50 ms RTT: the best of three fresh connections (the sender, the receiver and the delay line share this
// machine's cores, so one run can dip). Each connection is made through the delay, so its handshake measures the
// real RTT: a connection that measured ~1 ms before the delay appeared makes quic-go's HyStart leave slow start at
// once (40 KB window, 88-95 Mbit/s on Linux). This is the sender side on each OS: Linux sends in GSO batches, macOS
// and Windows one datagram at a time.
func TestQUICCarrierStreamHoldsTheBudgetAtRealRTT(t *testing.T) {
	if raceEnabled {
		t.Skip("throughput measurement: race instrumentation, not the carrier, bounds it")
	}
	var rates []float64
	for range 3 {
		rates = append(rates, quicStreamMbit(t))
		t.Logf("connection %d: %.1f Mbit/s", len(rates), rates[len(rates)-1])
	}
	sort.Float64s(rates)
	t.Logf("one QUIC stream at 50 ms RTT: %.1f Mbit/s best of %.1f (median %.1f, budget %.0f)", rates[2], rates, rates[1], throughputBudget)
	// CI runners share cores with the sender and the delay line, so the assertion is the best of three: it proves the
	// carrier can hold the budget when it is scheduled. The median on real hardware is checked by the device run on
	// victus (CHECKLIST PR-06, 219-225 Mbit/s).
	if rates[2] < throughputBudget {
		t.Fatalf("one QUIC stream at 50 ms RTT sustains at best %.1f Mbit/s (runs %.1f), budget %.0f", rates[2], rates, throughputBudget)
	}
}

// quicStreamMbit sends throughputBytes on one stream of a new connection through the 50 ms delay line.
func quicStreamMbit(t *testing.T) float64 {
	t.Helper()
	clientTLS, serverTLS, _ := testDataCarrierCertificates(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	packetConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.Listen(packetConn, serverTLS.Clone(), &quic.Config{
		MaxIdleTimeout: 15 * time.Second, MaxIncomingStreams: 16, MaxIncomingUniStreams: -1,
		InitialStreamReceiveWindow: 256 << 10, MaxStreamReceiveWindow: sharedprotocol.DataCarrierMaxStreamWindow,
		InitialConnectionReceiveWindow: 64 << 20, MaxConnectionReceiveWindow: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close() // quic.Listen leaves a socket it was given open
	defer listener.Close()
	received := make(chan error, 1)
	go func() {
		connection, err := listener.Accept(ctx)
		if err != nil {
			received <- err
			return
		}
		stream, err := connection.AcceptStream(ctx)
		if err != nil {
			received <- err
			return
		}
		n, err := io.Copy(io.Discard, stream)
		if err == nil && n != throughputBytes {
			err = io.ErrUnexpectedEOF
		}
		received <- err
	}()
	address := delayedUDPRelay(t, packetConn.LocalAddr().(*net.UDPAddr), 50*time.Millisecond)
	connection, err := quicdial.Dial(ctx, address, clientTLS, defaultQUICConfig(), false) // the daemon's own dial (data_carrier_endpoint.go)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseWithError(0, "")
	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	chunk := make([]byte, 64<<10)
	for sent := 0; sent < throughputBytes; sent += len(chunk) {
		if _, err := stream.Write(chunk); err != nil {
			t.Fatalf("write after %d bytes: %v", sent, err)
		}
	}
	_ = stream.Close()
	if err := <-received; err != nil {
		t.Fatalf("receive: %v", err)
	}
	return float64(throughputBytes) * 8 / time.Since(started).Seconds() / 1e6
}
