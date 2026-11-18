//go:build darwin || linux

package localapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

// F18-06: exec/ssh on a machine outside an API token's scope is refused by the control plane
// when the daemon asks for the descriptor; the command must say so, not "local service failed".
func TestLocalAPIPeerStreamOpenReportsAnAPITokenScopeRefusal(t *testing.T) {
	socket := filepath.Join(localAPITestDir(t), "scope.sock")
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(),
		Source: snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }),
		PeerStreams: peerStreamBrokerFunc(func(context.Context, Peer, PeerStreamRequest) (net.Conn, error) {
			return nil, fmt.Errorf("issue exec descriptor: %w", &api.APIError{Status: http.StatusForbidden, Code: TokenScopeCode, Message: "raw server text"})
		})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	waitForSocket(t, socket)
	client, err := NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewPeerStreamRequest("machine_1", "environment_1", 2, "exec", "operation_1", "credential", time.Now().Add(time.Minute), 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.OpenPeerStream(context.Background(), request)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusForbidden || remote.Code != TokenScopeCode || remote.Message != api.PublicMessage(TokenScopeCode) || remote.Message == "" {
		t.Fatalf("error = %#v", err)
	}
}

// F14-07: a below-floor client's exec/ssh is refused by the control plane (426 update_required) when the daemon asks for the
// descriptor; the command must show the floor, not 'peer stream unavailable'.
func TestLocalAPIPeerStreamOpenReportsAnUpdateRequiredRefusal(t *testing.T) {
	socket := filepath.Join(localAPITestDir(t), "floor.sock")
	floor := "Update required: Paperboat 2026.11.20.0 or newer. This Paperboat version is no longer supported. Run `pb update` to continue."
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(),
		Source: snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }),
		PeerStreams: peerStreamBrokerFunc(func(context.Context, Peer, PeerStreamRequest) (net.Conn, error) {
			return nil, fmt.Errorf("issue exec descriptor: %w", &api.APIError{Status: http.StatusUpgradeRequired, Code: UpdateRequiredCode, Message: floor})
		})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	waitForSocket(t, socket)
	client, err := NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewPeerStreamRequest("machine_1", "environment_1", 2, "exec", "operation_1", "credential", time.Now().Add(time.Minute), 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.OpenPeerStream(context.Background(), request)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusUpgradeRequired || remote.Code != UpdateRequiredCode || remote.Message != floor || !Refusal(remote.Code) {
		t.Fatalf("error = %#v", err)
	}
}
