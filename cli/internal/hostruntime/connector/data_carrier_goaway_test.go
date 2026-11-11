package connector

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// goawayHost is a one-carrier pool whose dials each reach a new edge carrier, sent on edges, and
// a consumer like the preview hub: it reads each edge stream's preface, hands a goaway to the
// pool and queues the others on accepted.
type goawayHost struct {
	pool       *DataCarrierPool
	edges      chan *DataCarrier
	accepted   chan acceptedStream
	mu         sync.Mutex
	transports []Transport
}

type acceptedStream struct {
	stream *DataCarrierStream
	open   StreamOpen
}

func (h *goawayHost) dials() []Transport {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Transport(nil), h.transports...)
}

// fail makes a dial (counted from 1) fail.
func newGoawayHost(t *testing.T, fail func(dial int) bool) *goawayHost {
	t.Helper()
	config := testDataCarrierConfig()
	config.MaximumStreams, config.AcceptBacklog = 8, 8
	poolConfig := DataCarrierPoolConfig{MaximumCarriers: 1, QueueDepth: 4, Preferred: QUIC, Fallback: TCPMux, EdgeID: "edge-a", EdgeProcessEpoch: "epoch-a", FailureDomains: []string{"edge-a"}, Session: testDataCarrierIdentity(), Carrier: config}
	h := &goawayHost{edges: make(chan *DataCarrier, 8), accepted: make(chan acceptedStream, 8)}
	dialer := func(_ context.Context, request DataCarrierDialRequest) (DataCarrierDialResult, error) {
		h.mu.Lock()
		h.transports = append(h.transports, request.Transport)
		dial := len(h.transports)
		h.mu.Unlock()
		if fail != nil && fail(dial) {
			return DataCarrierDialResult{}, &TransportDialError{Transport: request.Transport, Err: errors.New("edge refused"), Fallback: false}
		}
		local, remote := net.Pipe()
		edge, err := NewDataCarrierServer(context.Background(), remote, config, DataCarrierAdmission{Identity: testDataCarrierIdentity(), Authorize: func(context.Context, StreamOpen) error { return nil }})
		if err != nil {
			_ = local.Close()
			_ = remote.Close()
			return DataCarrierDialResult{}, err
		}
		t.Cleanup(func() { _ = edge.Close() })
		h.edges <- edge
		return DataCarrierDialResult{Link: local, PeerIdentity: request.Identity, Transport: request.Transport, EdgeID: request.EdgeID, FailureDomain: request.FailureDomain}, nil
	}
	pool, err := NewDataCarrierPool(context.Background(), poolConfig, dialer)
	if err != nil {
		t.Fatal(err)
	}
	h.pool = pool
	t.Cleanup(func() { _ = pool.Close() })
	if err := pool.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			stream, _, err := pool.AcceptStream(context.Background())
			if err != nil {
				return
			}
			open, err := connectorprotocol.ReadStreamOpen(stream)
			if err != nil {
				_ = stream.Close()
				continue
			}
			if open.Kind == connectorprotocol.Goaway {
				go stream.HandleGoaway()
				continue
			}
			h.accepted <- acceptedStream{stream, open}
		}
	}()
	return h
}

func (h *goawayHost) accept(t *testing.T) (*DataCarrierStream, StreamOpen) {
	t.Helper()
	select {
	case got := <-h.accepted:
		t.Cleanup(func() { _ = got.stream.Close() })
		return got.stream, got.open
	case <-time.After(2 * time.Second):
		t.Fatal("host accepted no stream")
		return nil, StreamOpen{}
	}
}

// edgeOpen is how the edge opens a stream: a raw stream, then its preface.
func edgeOpen(t *testing.T, edge *DataCarrier, open StreamOpen) io.ReadWriteCloser {
	t.Helper()
	stream, err := edge.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := connectorprotocol.WriteStreamOpen(stream, open); err != nil {
		t.Fatal(err)
	}
	return stream
}

// edgeHeldStream opens one stream from the edge that the host accepts and holds.
func edgeHeldStream(t *testing.T, h *goawayHost, edge *DataCarrier) (edgeSide io.ReadWriteCloser, hostSide *DataCarrierStream) {
	t.Helper()
	edgeSide = edgeOpen(t, edge, testDataCarrierStreamOpen("route-a", "held"))
	t.Cleanup(func() { _ = edgeSide.Close() })
	hostSide, _ = h.accept(t)
	return edgeSide, hostSide
}

// sendEdgeGoaway is the edge's drain notice (tunnel datacarrier): one goaway stream, then it waits
// (at most 1 s) for the host to close it.
func sendEdgeGoaway(t *testing.T, edge *DataCarrier) {
	t.Helper()
	sendGoaway(t, edge, func(w io.Writer) error {
		return connectorprotocol.WriteGoawayNotice(w, connectorprotocol.GoawayEdgeDraining, time.Now().Add(30*time.Second))
	})
}

func sendGoaway(t *testing.T, edge *DataCarrier, write func(io.Writer) error) {
	t.Helper()
	open := testDataCarrierStreamOpen("route-a", "goaway")
	open.Kind = connectorprotocol.Goaway
	stream := edgeOpen(t, edge, open)
	defer stream.Close()
	if err := write(stream); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { _, err := stream.Read(make([]byte, 1)); closed <- err }()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("host answered the goaway with data")
		}
	case <-time.After(time.Second):
		t.Fatal("host kept the goaway stream open")
	}
}

func roundTrip(t *testing.T, from io.Writer, to io.Reader, text string) {
	t.Helper()
	go func() { _, _ = io.WriteString(from, text) }()
	got := make([]byte, len(text))
	if _, err := io.ReadFull(to, got); err != nil || string(got) != text {
		t.Fatalf("held stream after goaway: %q, %v", got, err)
	}
}

// Edge install handover (make-before-break, as cloudflared replaces a connection that got
// GracefulShutdown): the edge says its carrier is draining while the host holds a stream on it. The
// host dials the replacement first (TCP first, the old edge process still owns UDP), opens new
// streams only there, the held stream keeps working, and the edge closing the drained carrier is
// no pool failure.
func TestEdgeGoawayReplacesTheCarrierBeforeNewStreamsMove(t *testing.T) {
	h := newGoawayHost(t, nil)
	draining := <-h.edges
	edgeSide, hostSide := edgeHeldStream(t, h, draining)

	sendEdgeGoaway(t, draining)
	var replacement *DataCarrier
	select {
	case replacement = <-h.edges:
	case <-time.After(2 * time.Second):
		t.Fatal("no replacement carrier after the edge goaway")
	}
	if got := h.dials(); len(got) != 2 || got[1] != TCPMux {
		t.Fatalf("dials = %v, want the replacement on TCP", got)
	}
	if !waitForDataCarrier(func() bool { return !h.pool.EdgeDraining() }) {
		t.Fatal("pool still has no carrier for new streams after the replacement")
	}
	opened, err := h.pool.OpenStream(t.Context(), testDataCarrierStreamOpen("route-a", "new"))
	if err != nil {
		t.Fatalf("new host stream: %v", err)
	}
	defer opened.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if stream, open, err := replacement.AcceptStream(ctx); err != nil || open.RequestID != "new" {
		t.Fatalf("new host stream not on the replacement: %+v, %v", open, err)
	} else {
		_ = stream.Close()
	}
	roundTrip(t, edgeSide, hostSide, "edge-to-host")
	roundTrip(t, hostSide, edgeSide, "host-to-edge")

	// New edge requests arrive on the replacement.
	newRequest := edgeOpen(t, replacement, testDataCarrierStreamOpen("route-a", "after"))
	defer newRequest.Close()
	if _, open := h.accept(t); open.RequestID != "after" {
		t.Fatalf("accepted %+v", open)
	}
	// The old edge process may still open a stream on the draining carrier until it closes it.
	late := edgeOpen(t, draining, testDataCarrierStreamOpen("route-a", "late"))
	defer late.Close()
	if _, open := h.accept(t); open.RequestID != "late" {
		t.Fatalf("accepted %+v", open)
	}

	// The old edge process ends the drained carrier: expected, the pool stays ready.
	_ = draining.Close()
	if !waitForDataCarrier(func() bool { h.pool.removeClosed(); return len(h.pool.Snapshot()) == 1 }) || h.pool.State() != DataCarrierPoolReady {
		t.Fatalf("pool after the drained carrier closed: %v %+v", h.pool.State(), h.pool.Snapshot())
	}
}

// No replacement can be dialed: the draining carrier still carries its held stream, the pool stays
// ready (no not-ready round cuts it) but says it only drains, and a new host stream fails at once
// instead of landing on the draining carrier, so the caller moves to the other edge.
func TestEdgeGoawayWithoutReplacementKeepsHeldStreamsAndRefusesNewOnes(t *testing.T) {
	h := newGoawayHost(t, func(dial int) bool { return dial > 1 })
	draining := <-h.edges
	edgeSide, hostSide := edgeHeldStream(t, h, draining)

	sendEdgeGoaway(t, draining)
	if !waitForDataCarrier(h.pool.EdgeDraining) {
		t.Fatal("pool did not take the goaway")
	}
	started := time.Now()
	if _, err := h.pool.OpenStream(t.Context(), testDataCarrierStreamOpen("route-a", "new")); !errors.Is(err, ErrDataCarrierUnavailable) {
		t.Fatalf("new stream with only a draining carrier: %v", err)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("refusing a new stream took %v", waited)
	}
	if h.pool.State() != DataCarrierPoolReady {
		t.Fatalf("pool state %v while its draining carrier serves", h.pool.State())
	}
	roundTrip(t, edgeSide, hostSide, "still-open")
}

// Revocation during the drain (the source closes the pool) ends the held stream at once.
func TestRevocationDuringEdgeDrainEndsHeldStreams(t *testing.T) {
	h := newGoawayHost(t, nil)
	draining := <-h.edges
	_, hostSide := edgeHeldStream(t, h, draining)
	sendEdgeGoaway(t, draining)
	<-h.edges
	_ = h.pool.Close()
	_ = hostSide.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := hostSide.Read(make([]byte, 1)); err == nil {
		t.Fatal("held stream read data after revocation")
	} else if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
		t.Fatal("held stream outlived the revocation")
	}
}

// A notice the edge may not send (host_draining) or a malformed one changes nothing.
func TestEdgeGoawayIgnoresOtherNotices(t *testing.T) {
	h := newGoawayHost(t, nil)
	edge := <-h.edges
	for _, write := range []func(io.Writer) error{
		func(w io.Writer) error {
			return connectorprotocol.WriteGoawayNotice(w, connectorprotocol.GoawayHostDraining, time.Now().Add(time.Minute))
		},
		func(w io.Writer) error { _, err := io.WriteString(w, "\x00\x00\x00\x04junk"); return err },
	} {
		sendGoaway(t, edge, write)
	}
	if h.pool.EdgeDraining() || len(h.dials()) != 1 {
		t.Fatalf("ignored notice changed the pool: draining=%v dials=%v", h.pool.EdgeDraining(), h.dials())
	}
}
