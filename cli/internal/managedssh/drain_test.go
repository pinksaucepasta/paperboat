package managedssh

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func openDrainBridge(t *testing.T) (*Host, net.Conn, <-chan error) {
	t.Helper()
	listener := loopbackListener(t)
	t.Cleanup(func() { listener.Close() })
	go echoConnections(listener)
	host, err := NewHost(HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.ReconcileTarget(t.Context(), 1, uint16(listener.Addr().(*net.TCPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close() })
	done := make(chan error, 1)
	go func() { _, err := host.Serve(t.Context(), 1, server); done <- err }()
	echoDrainBridge(t, client)
	return host, client, done
}
func echoDrainBridge(t *testing.T, client net.Conn) {
	t.Helper()
	client.SetDeadline(time.Now().Add(time.Second))
	if _, err := client.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(client, b[:]); err != nil || b[0] != 1 {
		t.Fatal("accepted SSH bridge lost bytes", err)
	}
	client.SetDeadline(time.Time{})
}
func waitDrainFence(t *testing.T, host *Host) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		if _, accepting := host.Target(); !accepting {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("drain did not fence new SSH bridges")
		case <-time.After(time.Millisecond):
		}
	}
}
func TestCarrierDrainKeepsAcceptedBridgeUntilItFinishes(t *testing.T) {
	host, client, bridge := openDrainBridge(t)
	drained := make(chan error, 1)
	go func() { drained <- host.Drain(t.Context()) }()
	waitDrainFence(t, host)
	if _, err := host.Serve(t.Context(), 1, pipeStream(t)); !errors.Is(err, ErrSSHHostStale) {
		t.Fatal("retiring carrier admitted another bridge", err)
	}
	echoDrainBridge(t, client)
	select {
	case err := <-drained:
		t.Fatal("drain ended before its accepted bridge", err)
	default:
	}
	client.Close()
	if err := <-bridge; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("finished bridge retained the carrier")
	}
}
func TestCancelledCarrierDrainPreservesBridgeForRollback(t *testing.T) {
	host, client, bridge := openDrainBridge(t)
	ctx, cancel := context.WithCancel(t.Context())
	drained := make(chan error, 1)
	go func() { drained <- host.Drain(ctx) }()
	waitDrainFence(t, host)
	cancel()
	if err := <-drained; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := host.ResumeAdmissions(); err != nil {
		t.Fatal(err)
	}
	echoDrainBridge(t, client)
	client.Close()
	if err := <-bridge; err != nil {
		t.Fatal(err)
	}
}
func TestCarrierDrainCapClosesLongLivedBridge(t *testing.T) {
	host, _, bridge := openDrainBridge(t)
	start := time.Now()
	if err := host.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < CarrierDrainTimeout || elapsed > CarrierDrainTimeout+carrierDrainCloseTimeout+time.Second {
		t.Fatal("carrier did not honor its bounded drain", elapsed)
	}
	if err := <-bridge; !errors.Is(err, context.Canceled) {
		t.Fatal("drain cap did not close the accepted bridge", err)
	}
	if _, accepting := host.Target(); accepting {
		t.Fatal("drained carrier resumed admission")
	}
}
