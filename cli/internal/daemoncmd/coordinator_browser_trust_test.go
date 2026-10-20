package daemoncmd

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"

	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
)

// A browser certificate (or its trust) that cannot be installed disables browser URLs only; machine
// names and native access to the machine's ports keep working.
func TestCoordinatorBrowserCertificateFailureKeepsNativeMachineAccess(t *testing.T) {
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{
		NameClient:  names,
		DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unexpected dial") },
		IssueCertificate: func(context.Context, string, string, int) (tls.Certificate, error) {
			return tls.Certificate{}, errors.New("browser trust is not installable here")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	peer := &pbSync.PeerUpdate{PeerId: "machine", Alias: "hp", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	for range 2 {
		if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
			t.Fatalf("browser certificate failure withdrew machine access: %v", err)
		}
	}
	names.mu.Lock()
	_, hasNames := names.names["hp."+testBrowserDomain]
	nativeAddress := names.addresses[8080]
	names.mu.Unlock()
	if !hasNames || nativeAddress == "" || len(c.routes) != 1 {
		t.Fatalf("names=%v native=%q routes=%d", hasNames, nativeAddress, len(c.routes))
	}
	if len(c.webRoutes) != 0 {
		t.Fatal("browser gateway was kept without a certificate")
	}
}
