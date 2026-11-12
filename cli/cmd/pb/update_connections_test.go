//go:build darwin || linux || windows

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

// F14-02 (X-36): an inbound `pb ssh` into this machine is carried by its carrier, not its daemon; the update check must
// count it, or `pb update` cuts a session of another machine without a word.
func TestOpenConnectionsForUpdateCountsInboundSSH(t *testing.T) {
	prevDaemon, prevCarrier := readDaemonConnections, readCarrierSSH
	t.Cleanup(func() { readDaemonConnections, readCarrierSSH = prevDaemon, prevCarrier })
	idle := func(context.Context) (liveconnections.Counts, error) { return liveconnections.Counts{}, nil }
	readDaemonConnections = idle
	readCarrierSSH = func(context.Context) (int, error) { return 2, nil }
	open, err := openConnectionsForUpdate(t.Context())
	if err != nil || open.SSH != 2 {
		t.Fatalf("an inbound session was not counted: %+v %v", open, err)
	}
	readDaemonConnections = func(context.Context) (liveconnections.Counts, error) {
		return liveconnections.Counts{SSH: 1, Forwards: 1}, nil
	}
	if open, _ = openConnectionsForUpdate(t.Context()); open.SSH != 3 || open.Forwards != 1 {
		t.Fatalf("both ends: %+v", open)
	}
	refusal := updateOpenConnectionsFromFile(t.Context())
	if refusal == nil {
		t.Fatal("a manual update was not refused while sessions are open")
	}
	readCarrierSSH = func(context.Context) (int, error) { return 0, errors.New("carrier down") }
	readDaemonConnections = func(context.Context) (liveconnections.Counts, error) {
		return liveconnections.Counts{}, errors.New("daemon down")
	}
	if open, err = openConnectionsForUpdate(t.Context()); err == nil || open.Total() != 0 {
		t.Fatalf("both probes down must report unconfirmed: %+v %v", open, err)
	}
}
