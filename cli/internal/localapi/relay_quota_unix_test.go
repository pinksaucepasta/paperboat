//go:build darwin || linux

package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type quotaTransportError struct{ message string }

func (e quotaTransportError) Error() string           { return e.message }
func (e quotaTransportError) LocalAPICode() string    { return RelayQuotaCode }
func (e quotaTransportError) LocalAPIMessage() string { return e.message }

// A connection that failed because the workspace's hosted relay allowance is used up reaches the
// command as the plan-limit refusal, not as a generic peer transport failure.
func TestLocalAPIPeerStreamOpenReportsAnExhaustedRelayAllowance(t *testing.T) {
	const message = "This workspace's hosted relay and edge bandwidth is used up. Add bandwidth: https://console.example.test/billing"
	socket := filepath.Join(localAPITestDir(t), "quota.sock")
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(),
		Source: snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }),
		PeerStreams: peerStreamBrokerFunc(func(context.Context, Peer, PeerStreamRequest) (net.Conn, error) {
			return nil, fmt.Errorf("dial exec: %w", quotaTransportError{message})
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
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusTooManyRequests || remote.Code != RelayQuotaCode || remote.Message != message {
		t.Fatalf("error = %#v", err)
	}
}

// F13-03: a probe (pb ping) that timed out because relays are withheld names the plan limit, not a
// deadline.
func TestPeerProbeTimedOutWithRelaysWithheldNamesThePlanLimit(t *testing.T) {
	const message = "Hosted relay bandwidth is used up. Add bandwidth or use your own relay."
	request, err := NewPeerStreamRequest("machine_test", "environment_test", 1, "health_probe", "operation_test", "credential_test", time.Now().Add(time.Minute), 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	input := httptest.NewRequest(http.MethodPost, "/v1/peer-probes", bytes.NewReader(body))
	input.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	cause := errors.Join(quotaTransportError{message}, context.DeadlineExceeded)
	server := &Server{config: ServerConfig{PeerProbes: failingProbe{cause}}}
	server.peerProbe(response, input, localRequestID(), Peer{})
	remote, ok := decodeRemoteErrorReader(response.Code, response.Body).(*RemoteError)
	if !ok || response.Code != http.StatusTooManyRequests || remote.Code != RelayQuotaCode || remote.Message != message {
		t.Fatalf("status %d remote %#v", response.Code, remote)
	}
}
