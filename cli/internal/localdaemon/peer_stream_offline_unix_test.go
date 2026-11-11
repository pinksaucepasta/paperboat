//go:build darwin || linux

package localdaemon

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// F21-11: a request the client marked offline goes straight to the transport (the host admits it from its signed
// scopes), while a pending request (no credential, not offline) still goes through descriptor issuance.
func TestPeerStreamBrokerOpensAnOfflineRequestWithoutIssuingADescriptor(t *testing.T) {
	var issued, opened []localapi.PeerStreamRequest
	broker := peerStreamBroker{
		issue: func(_ context.Context, request localapi.PeerStreamRequest) (localapi.PeerStreamRequest, error) {
			issued = append(issued, request)
			return request, errors.New("Paperboat is unreachable")
		},
		open: func(_ context.Context, _ localapi.Peer, request localapi.PeerStreamRequest) (net.Conn, error) {
			opened = append(opened, request)
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
	}
	base := localapi.PeerStreamRequest{Schema: localapi.PeerStreamSchemaV1, MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1,
		Consumer: "terminal", OperationID: "terminal_offline_1", Deadline: time.Now().Add(time.Hour), MaximumBytes: 1 << 20}
	offline := base
	offline.Offline = true
	conn, err := broker.OpenPeerStream(context.Background(), localapi.Peer{}, offline)
	if err != nil {
		t.Fatalf("offline request: %v", err)
	}
	_ = conn.Close()
	if len(issued) != 0 || len(opened) != 1 || !opened[0].Offline {
		t.Fatalf("issued %d, opened %+v: an offline request must not ask Paperboat for a descriptor", len(issued), opened)
	}
	if _, err := broker.OpenPeerStream(context.Background(), localapi.Peer{}, base); err == nil || len(issued) != 1 {
		t.Fatalf("a pending request skipped issuance: err=%v issued=%d", err, len(issued))
	}
}
