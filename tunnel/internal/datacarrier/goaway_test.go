package datacarrier

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// Audit T10: a host that is going away says so on its carrier; the edge then opens no new
// streams on it while the streams already open keep working. A notice the host may not send
// (edge_draining) or a malformed one changes nothing.
func TestHostGoawayDrainsTheCarrierAndKeepsOpenStreams(t *testing.T) {
	edge, connector := dataCarrierPair(t, testCarrierConfig(4))
	draining := edge.Draining()
	accepted := make(chan *Stream, 1)
	go func() {
		stream, _, err := connector.AcceptStream(context.Background())
		if err == nil {
			accepted <- stream
		}
	}()
	open, err := edge.OpenStream(context.Background(), testStreamOpen("route-a", "before-goaway"))
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	hostSide := <-accepted
	defer hostSide.Close()

	goaway := func(id string, write func(io.Writer) error) {
		t.Helper()
		request := testStreamOpen("route-a", id)
		request.Kind = connectorprotocol.Goaway
		stream, err := connector.OpenStream(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if err := write(stream); err != nil {
			t.Fatal(err)
		}
		// The edge closes the stream once it has the notice.
		_ = stream.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := stream.Read(make([]byte, 1)); err == nil {
			t.Fatal("edge answered a goaway with data")
		} else if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
			t.Fatal("edge kept the goaway stream open")
		}
		_ = stream.Close()
	}
	notice := func(reason string) func(io.Writer) error {
		return func(w io.Writer) error {
			return connectorprotocol.WriteGoawayNotice(w, reason, time.Now().Add(5*time.Second))
		}
	}
	goaway("goaway-edge", notice(connectorprotocol.GoawayEdgeDraining))
	goaway("goaway-junk", func(w io.Writer) error { _, err := io.WriteString(w, "\x00\x00\x00\x04junk"); return err })
	select {
	case <-draining:
		t.Fatal("a notice the host may not send drained the carrier")
	case <-time.After(200 * time.Millisecond):
	}
	goaway("goaway-host", notice(connectorprotocol.GoawayHostDraining))
	select {
	case <-draining:
	case <-time.After(2 * time.Second):
		t.Fatal("host goaway did not drain the carrier")
	}
	if _, err := edge.OpenStream(context.Background(), testStreamOpen("route-a", "after-goaway")); !errors.Is(err, ErrCarrierDraining) {
		t.Fatalf("new stream on a draining carrier: %v", err)
	}
	go func() { _, _ = io.WriteString(open, "still-open") }()
	got := make([]byte, len("still-open"))
	_ = hostSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(hostSide, got); err != nil || string(got) != "still-open" {
		t.Fatalf("open stream after goaway: %q, %v", got, err)
	}
	_ = open.Close()
	_ = hostSide.Close()
	if !waitForCarrier(func() bool { return edge.ActiveStreams() == 0 && connector.ActiveStreams() == 0 }) {
		t.Fatalf("stream permit leaked: edge=%d connector=%d", edge.ActiveStreams(), connector.ActiveStreams())
	}
}

// Audit T10 (prod 2026-10-26): a goaway is never permanent. A carrier the host keeps open past the
// goaway's deadline accepts new streams again; the deadline is capped at maximumHostDrain.
func TestHostGoawayEndsAtItsDeadline(t *testing.T) {
	edge, _ := dataCarrierPair(t, testCarrierConfig(4))
	now := time.Now()
	edge.drain(now.Add(time.Hour), now)
	if until := edge.DrainUntil(); until.After(now.Add(maximumHostDrain)) {
		t.Fatalf("goaway lasts until %v, beyond the %v cap", until, maximumHostDrain)
	}
	if !edge.drainingNow(now.Add(time.Second)) {
		t.Fatal("carrier not draining during its goaway")
	}
	if edge.EndDrain(now.Add(time.Second)) {
		t.Fatal("goaway ended before its deadline")
	}
	end := edge.DrainUntil()
	if !edge.EndDrain(end) || edge.drainingNow(end) {
		t.Fatal("goaway did not end at its deadline")
	}
	select {
	case <-edge.Draining():
		t.Fatal("a fresh Draining channel is already closed")
	default:
	}
	edge.drain(now.Add(-time.Second), now)
	if edge.drainingNow(now) {
		t.Fatal("a goaway already past its deadline drained the carrier")
	}
}
