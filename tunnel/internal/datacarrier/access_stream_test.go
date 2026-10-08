package datacarrier

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
)

func TestServerAcceptAccessStreamAuthenticatesCarrierAndLeavesPayloadOpaque(t *testing.T) {
	edge, connector := dataCarrierPair(t, testCarrierConfig(4))
	open := testStreamOpen("route-a", "access-request")
	open.Kind = AccessStreamHTTPS
	accepted := make(chan struct {
		stream *Stream
		open   StreamOpen
		err    error
	}, 1)
	go func() {
		stream, metadata, err := edge.AcceptAccessStream(context.Background())
		accepted <- struct {
			stream *Stream
			open   StreamOpen
			err    error
		}{stream: stream, open: metadata, err: err}
	}()
	source, err := connector.OpenStream(context.Background(), open)
	if err != nil {
		t.Fatal(err)
	}
	result := <-accepted
	if result.err != nil || result.stream == nil || result.open != open {
		t.Fatalf("accepted stream = %+v, err=%v", result.open, result.err)
	}
	const payload = "opaque access bytes"
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(source, payload)
		writeDone <- err
	}()
	got, readErr := io.ReadAll(io.LimitReader(result.stream, int64(len(payload))))
	if readErr != nil || string(got) != payload {
		t.Fatalf("payload = %q, read error = %v", got, readErr)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	_ = source.Close()
	_ = result.stream.Close()
	if !waitForCarrier(func() bool { return edge.ActiveStreams() == 0 && connector.ActiveStreams() == 0 }) {
		t.Fatalf("stream permit leaked: edge=%d connector=%d", edge.ActiveStreams(), connector.ActiveStreams())
	}
}

func TestServerAcceptAccessStreamRejectsUnsupportedKindBeforePayload(t *testing.T) {
	edge, connector := dataCarrierPair(t, testCarrierConfig(4))
	open := testStreamOpen("route-a", "public-kind")
	// http is valid connector-v1, but it is not the private access seam.
	raw, err := connector.session.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	acceptDone := make(chan error, 1)
	go func() {
		_, _, err := edge.AcceptAccessStream(context.Background())
		acceptDone <- err
	}()
	if err := connectorprotocol.WriteStreamOpen(raw, open); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acceptDone:
		if !errors.Is(err, ErrAccessStreamKind) {
			t.Fatalf("accept error = %v, want %v", err, ErrAccessStreamKind)
		}
	case <-time.After(time.Second):
		t.Fatal("access stream accept did not reject unsupported kind")
	}
	_ = raw.Close()
}

func TestServerAcceptAccessStreamRejectsStaleCarrierIdentity(t *testing.T) {
	edge, connector := dataCarrierPair(t, testCarrierConfig(4))
	open := testStreamOpen("route-a", "stale-identity")
	open.Kind = AccessStreamTCP
	open.SessionID = "session-stale"
	raw, err := connector.session.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	acceptDone := make(chan error, 1)
	go func() {
		_, _, err := edge.AcceptAccessStream(context.Background())
		acceptDone <- err
	}()
	if err := connectorprotocol.WriteStreamOpen(raw, open); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acceptDone:
		if !errors.Is(err, ErrAccessStreamIdentity) {
			t.Fatalf("accept error = %v, want %v", err, ErrAccessStreamIdentity)
		}
	case <-time.After(time.Second):
		t.Fatal("stale identity accept did not return")
	}
	_ = raw.Close()
}

func TestServerDemultiplexesPrivateAccessAndBrowserTerminalOutput(t *testing.T) {
	edge, connector := dataCarrierPair(t, testCarrierConfig(8))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type accepted struct {
		open   StreamOpen
		stream *Stream
		err    error
	}
	accessResult := make(chan accepted, 1)
	outputResult := make(chan accepted, 1)
	go func() {
		stream, open, err := edge.AcceptAccessStream(ctx)
		accessResult <- accepted{open: open, stream: stream, err: err}
	}()
	go func() {
		stream, open, err := edge.AcceptBrowserTerminalOutputStream(ctx)
		outputResult <- accepted{open: open, stream: stream, err: err}
	}()

	outputOpen := testStreamOpen("route-a", "term_1")
	outputOpen.Kind = BrowserTerminalOutputStream
	output, err := connector.OpenStream(ctx, outputOpen)
	if err != nil {
		t.Fatal(err)
	}
	accessOpen := testStreamOpen("route-a", "access_1")
	accessOpen.Kind = AccessStreamHTTPS
	access, err := connector.OpenStream(ctx, accessOpen)
	if err != nil {
		t.Fatal(err)
	}

	var gotAccess, gotOutput accepted
	select {
	case gotAccess = <-accessResult:
	case <-ctx.Done():
		t.Fatal("private access stream was not demultiplexed")
	}
	select {
	case gotOutput = <-outputResult:
	case <-ctx.Done():
		t.Fatal("terminal output stream was not demultiplexed")
	}
	if gotAccess.err != nil || gotAccess.stream == nil || gotAccess.open != accessOpen {
		t.Fatalf("private access result=%+v err=%v", gotAccess.open, gotAccess.err)
	}
	if gotOutput.err != nil || gotOutput.stream == nil || gotOutput.open != outputOpen {
		t.Fatalf("terminal output result=%+v err=%v", gotOutput.open, gotOutput.err)
	}
	_ = gotAccess.stream.Close()
	_ = gotOutput.stream.Close()
	_ = access.Close()
	_ = output.Close()
}
