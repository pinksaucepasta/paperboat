package daemoncmd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

// guardDomainClient answers like the machine guard: it reports the domain
// recorded for the connection's owner.
type guardDomainClient struct {
	*coordinatorNameClient
	domain string
	err    error
}

func (c *guardDomainClient) Status(context.Context) (machineguard.RuntimeStatus, error) {
	return machineguard.RuntimeStatus{BrowserDomain: c.domain}, c.err
}

func newUnsetDomainCoordinator(t *testing.T, client guardedNameClient) *Coordinator {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(CoordinatorConfig{SocketAddress: filepath.Join(dir, "daemon.sock"), NameClient: client, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") }})
	if err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func TestCoordinatorUsesTheDomainTheGuardRecordsWhenConfigurationHasNone(t *testing.T) {
	names := &guardDomainClient{coordinatorNameClient: &coordinatorNameClient{}, domain: "ppr.example.test"}
	coordinator := newUnsetDomainCoordinator(t, names)
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	coordinator.applySnapshot([]*pbSync.PeerUpdate{peer}, 1)
	names.mu.Lock()
	_, present := names.names["studio.ppr.example.test"]
	names.mu.Unlock()
	if !present {
		t.Fatal("machine name does not use the guard's local access domain")
	}
	// A reinstall that records another domain is followed without restarting the daemon.
	names.domain = "other.example.test"
	coordinator.applySnapshot([]*pbSync.PeerUpdate{peer}, 2)
	names.mu.Lock()
	_, present = names.names["studio.other.example.test"]
	names.mu.Unlock()
	if !present {
		t.Fatal("daemon kept a stale local access domain")
	}
}

func TestCoordinatorServesNothingWithoutALocalAccessDomain(t *testing.T) {
	names := &guardDomainClient{coordinatorNameClient: &coordinatorNameClient{}}
	coordinator := newUnsetDomainCoordinator(t, names)
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	coordinator.applySnapshot([]*pbSync.PeerUpdate{peer}, 1)
	names.mu.Lock()
	published := len(names.names)
	names.mu.Unlock()
	if published != 0 {
		t.Fatalf("published %d names without a local access domain", published)
	}
}
