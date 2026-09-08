package peerquic_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
)

var tailcatOperationTimeout = flag.Duration("tailcat-operation-timeout", 30*time.Second, "Tailcat stream workload deadline; allow instrumentation overhead under -race")

// Tests use real WireGuard/netstack UDP and Paperboat's actual QUIC entrypoints.
// The in-process upstream DERP/STUN fixture binds only remote-host loopback.
func TestTailcatQUIC(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	var maximum atomic.Uint32
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	keys := []key.NodePrivate{key.NewNode(), key.NewNode()}
	allowed := []key.NodePublic{keys[0].Public(), keys[1].Public()}
	server, err := tailnet.ListenUDP(dm.Regions[1], allowed, 4242)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	clientTLS, serverTLS := probeTLSConfigs(t)
	// Unlike the historical probe helper, acceptance pins both real certificates.
	pin := func(want []byte) func(tls.ConnectionState) error {
		return func(s tls.ConnectionState) error {
			if len(s.PeerCertificates) != 1 || !bytes.Equal(s.PeerCertificates[0].Raw, want) {
				return errors.New("unexpected endpoint certificate")
			}
			return nil
		}
	}
	clientTLS.VerifyConnection = pin(serverTLS.Certificates[0].Certificate[0])
	serverTLS.VerifyConnection = pin(clientTLS.Certificates[0].Certificate[0])
	config := peerquic.DevelopmentSessionConfig(peerquic.ClassTransfer)
	clients := make([]*tailnet.UDPClient, 2)
	for i := range clients {
		clients[i], err = tailnet.NewUDPClient(server.Address(), keys[i], 4242)
		if err != nil {
			t.Fatal(err)
		}
		defer clients[i].Close()
	}
	// Each admitted UDP flow gets its own QUIC listener concurrently. Late UDP
	// close packets can create a flow after its old socket was released; they must
	// not block admission of a new connection behind a serial handshake wait.
	serving, stopServing := context.WithCancel(t.Context())
	accepted := make(chan *peerquic.Session, tailnet.MaxFlows)
	var handlers sync.WaitGroup
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			flow, e := server.Accept(serving)
			if e != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				listener, e := peerquic.ListenPacket(&measuredTailcatPacket{Packet: flow, maximum: &maximum}, serverTLS, config)
				if e != nil {
					return
				}
				defer listener.Close()
				handshake, stop := context.WithTimeout(serving, 10*time.Second)
				session, e := listener.Accept(handshake)
				stop()
				if e != nil {
					return
				}
				defer session.Close()
				select {
				case accepted <- session:
				case <-serving.Done():
					return
				}
				select {
				case <-session.Connection.Context().Done():
				case <-serving.Done():
				}
			}()
		}
	}()
	defer func() {
		stopServing()
		_ = server.Close()
		<-stopped
		done := make(chan struct{})
		go func() { handlers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("QUIC listener workers leaked")
		}
	}()
	badTLS := clientTLS.Clone()
	badTLS.VerifyConnection = func(tls.ConnectionState) error { return errors.New("wrong pinned identity") }
	denied, stopDenied := context.WithTimeout(t.Context(), 3*time.Second)
	badSocket, e := clients[0].Dial(denied)
	if e != nil {
		stopDenied()
		t.Fatal(e)
	}
	if session, e := peerquic.DialPacket(denied, badSocket, badTLS, config); e == nil {
		session.Close()
		t.Fatal("wrong QUIC identity accepted")
	}
	stopDenied()
	if _, e := badSocket.Write([]byte{1}); e == nil {
		t.Fatal("failed QUIC setup retained socket")
	}
	for round := range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), *tailcatOperationTimeout)
		sessions := make([]*peerquic.Session, 0, 4)
		cleanup := func() {
			for _, s := range sessions {
				_ = s.Close()
			}
			cancel()
		}
		for peer, client := range clients {
			t.Logf("round=%d peer=%d handshake", round, peer)
			socket, err := client.Dial(ctx)
			if err != nil {
				cleanup()
				t.Fatal(err)
			}
			outgoing, e := peerquic.DialPacket(ctx, &measuredTailcatPacket{Packet: socket, maximum: &maximum}, clientTLS, config)
			if e != nil {
				cleanup()
				t.Fatal(e)
			}
			sessions = append(sessions, outgoing)
			select {
			case incoming := <-accepted:
				sessions = append(sessions, incoming)
			case <-ctx.Done():
				cleanup()
				t.Fatal(ctx.Err())
			}
		}
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		// Both peers concurrently originate four streams, each carrying 8 MiB in
		// each direction. Verify bytes incrementally, without retaining full payloads.
		for i := 0; i < len(sessions); i += 2 {
			a, b := sessions[i], sessions[i+1]
			for range 4 {
				wg.Add(2)
				go func() {
					defer wg.Done()
					stream, e := a.Connection.OpenStreamSync(ctx)
					if e == nil {
						e = exchangeTailcat(stream, byte(round+1))
					}
					errs <- e
				}()
				go func() {
					defer wg.Done()
					stream, e := b.Connection.AcceptStream(ctx)
					if e == nil {
						e = exchangeTailcat(stream, byte(round+1))
					}
					errs <- e
				}()
			}
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			if e != nil {
				cleanup()
				t.Fatal(e)
			}
		}
		// Cancellation is operation-local and cannot kill another peer's session.
		canceled, stop := context.WithCancel(ctx)
		stop()
		if _, e := sessions[0].Connection.AcceptStream(canceled); !errors.Is(e, context.Canceled) {
			cleanup()
			t.Fatalf("accept cancellation: %v", e)
		}
		started := time.Now()
		cleanup()
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("cleanup took %s", elapsed)
		}
		if round == 0 {
			_ = clients[0].Close()
			clients[0], err = tailnet.NewUDPClient(server.Address(), keys[0], 4242)
			if err != nil {
				t.Fatal(err)
			}
			defer clients[0].Close()
		}
	}
	if n := maximum.Load(); n < 1200 || n > tailnet.MaxPayload {
		t.Fatalf("observed QUIC maximum packet %d", n)
	}
	t.Logf("maximum QUIC packet=%d bytes; two peers, 4 streams each, 8 MiB each direction, 2 rounds", maximum.Load())
}

type measuredTailcatPacket struct {
	*tailnet.Packet
	maximum *atomic.Uint32
}

func (p *measuredTailcatPacket) WriteTo(b []byte, a net.Addr) (int, error) {
	for old := p.maximum.Load(); uint32(len(b)) > old; old = p.maximum.Load() {
		if p.maximum.CompareAndSwap(old, uint32(len(b))) {
			break
		}
	}
	return p.Packet.WriteTo(b, a)
}

type tailcatStream interface {
	io.Reader
	io.Writer
	Close() error
	SetDeadline(time.Time) error
}

func exchangeTailcat(s tailcatStream, value byte) error {
	if err := s.SetDeadline(time.Now().Add(*tailcatOperationTimeout - 5*time.Second)); err != nil {
		return err
	}
	const blocks = 256
	payload := bytes.Repeat([]byte{value}, 32<<10)
	written := make(chan error, 1)
	go func() {
		for range blocks {
			if _, err := s.Write(payload); err != nil {
				written <- err
				return
			}
		}
		written <- s.Close()
	}()
	buf := make([]byte, len(payload))
	var readErr error
	for range blocks {
		if _, readErr = io.ReadFull(s, buf); readErr != nil {
			break
		}
		if !bytes.Equal(buf, payload) {
			readErr = fmt.Errorf("stream integrity mismatch")
			break
		}
	}
	if readErr == nil {
		var extra [1]byte
		n, e := s.Read(extra[:])
		if n != 0 || e != io.EOF {
			readErr = fmt.Errorf("stream boundary: %d %v", n, e)
		}
	}
	return errors.Join(readErr, <-written)
}

func TestTailcatPacketContract(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	if _, err := tailnet.ListenUDP(dm.Regions[1], nil, 4242); !errors.Is(err, tailnet.ErrAdmission) {
		t.Fatal("empty policy accepted")
	}
	k := key.NewNode()
	server, err := tailnet.ListenUDP(dm.Regions[1], []key.NodePublic{k.Public()}, 4242)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := tailnet.NewUDPClient(server.Address(), k, 4242)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), *tailcatOperationTimeout)
	defer cancel()
	a, err := client.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err = a.Write(make([]byte, tailnet.MaxPayload+1)); !errors.Is(err, tailnet.ErrPayloadTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	if _, err = a.WriteTo([]byte{1}, &net.UDPAddr{IP: net.IPv6loopback, Port: 7}); !errors.Is(err, tailnet.ErrWrongPeer) {
		t.Fatalf("wrong destination: %v", err)
	}
	payload := bytes.Repeat([]byte{42}, tailnet.MaxPayload)
	if _, err = a.Write(payload); err != nil {
		t.Fatal(err)
	}
	b, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, tailnet.MaxPayload+1)
	n, source, err := b.ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], payload) || source.String() != a.LocalAddr().String() || b.LocalAddr().String() != a.RemoteAddr().String() {
		t.Fatalf("datagram/source/port mismatch: %d %v", n, err)
	}
	_ = b.SetReadDeadline(time.Now().Add(-time.Second))
	_, _, err = b.ReadFrom(buf)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("read deadline: %v", err)
	}
	_ = b.SetReadDeadline(time.Time{})
	_ = b.SetWriteDeadline(time.Now().Add(-time.Second))
	_, err = b.Write(payload)
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("write deadline: %v", err)
	}
	_ = b.SetWriteDeadline(time.Time{})
	if _, err = b.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = a.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err = a.ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("deadline recovery: %v", err)
	}
	_ = b.SetReadDeadline(time.Time{})
	done := make(chan error, 1)
	go func() { _, _, e := b.ReadFrom(buf); done <- e }()
	_ = b.Close()
	_ = b.Close()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("close returned successful read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close failed to unblock read")
	}
	// Outgoing admission is bounded and released sockets restore capacity.
	flows := make([]*tailnet.Packet, 0, tailnet.MaxFlows-1)
	for range tailnet.MaxFlows - 1 {
		p, e := client.Dial(ctx)
		if e != nil {
			t.Fatal(e)
		}
		flows = append(flows, p)
	}
	if _, e := client.Dial(ctx); !errors.Is(e, tailnet.ErrFlowLimit) {
		t.Fatalf("flow limit: %v", e)
	}
	for _, p := range flows {
		_ = p.Close()
	}
	p, e := client.Dial(ctx)
	if e != nil {
		t.Fatalf("capacity recovery: %v", e)
	}
	_ = p.Close()
}
