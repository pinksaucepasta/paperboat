package connector

import (
	"context"
	"net"
	"testing"
	"time"
)

// A stream cut has to be explainable: every way a carrier ends records one finite reason.
func TestDataCarrierRecordsWhyItClosed(t *testing.T) {
	config := DefaultDataCarrierConfig()

	t.Run("peer ends the session", func(t *testing.T) {
		client, server := dataCarrierPair(t, config)
		if reason := client.CloseReason(); reason != "" {
			t.Fatalf("an open carrier reports reason %q", reason)
		}
		_ = server.Close()
		if !waitForDataCarrier(func() bool { return client.closed() }) {
			t.Fatal("the client did not notice the peer closing")
		}
		if reason := client.CloseReason(); reason != CloseSessionClosed && reason != CloseSessionError {
			t.Fatalf("peer close recorded %q, want session_closed or session_error", reason)
		}
	})

	t.Run("caller closes", func(t *testing.T) {
		client, _ := dataCarrierPair(t, config)
		_ = client.Close()
		if reason := client.CloseReason(); reason != CloseLocal {
			t.Fatalf("Close recorded %q, want %q", reason, CloseLocal)
		}
	})

	t.Run("the first reason wins", func(t *testing.T) {
		client, _ := dataCarrierPair(t, config)
		_ = client.closeFor(ClosePoolShutdown)
		_ = client.closeFor(CloseHealthPingMissed)
		_ = client.Close()
		if reason := client.CloseReason(); reason != ClosePoolShutdown {
			t.Fatalf("recorded %q, want the first reason %q", reason, ClosePoolShutdown)
		}
	})

	t.Run("target removed", func(t *testing.T) {
		client, _ := dataCarrierPair(t, config)
		_ = client.closeFor(CloseTargetRemoved)
		if reason := client.CloseReason(); reason != CloseTargetRemoved {
			t.Fatalf("recorded %q, want %q", reason, CloseTargetRemoved)
		}
	})
}

func TestDataCarrierContextEndIsRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	local, remote := net.Pipe()
	defer remote.Close()
	carrier, err := NewDataCarrierClient(ctx, local, DefaultDataCarrierConfig(), testDataCarrierIdentity())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for !carrier.closed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if reason := carrier.CloseReason(); reason != CloseContextDone {
		t.Fatalf("context end recorded %q, want %q", reason, CloseContextDone)
	}
}
