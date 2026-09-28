//go:build darwin || linux

package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type failingProbe struct{ err error }

func (p failingProbe) ProbePeer(context.Context, Peer, PeerStreamRequest) (PeerProbeResult, error) {
	return PeerProbeResult{}, p.err
}

type reportingProbe struct{ result PeerProbeResult }

func (p reportingProbe) ProbePeer(context.Context, Peer, PeerStreamRequest) (PeerProbeResult, error) {
	return p.result, nil
}

func TestProbeReportsNativeConnectionPath(t *testing.T) {
	request, err := NewPeerStreamRequest("machine_test", "environment_test", 1, "health_probe", "operation_test", "credential_test", time.Now().Add(time.Minute), 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"direct", "peer_relay", "regional_relay", "unknown"} {
		input := httptest.NewRequest(http.MethodPost, "/v1/peer-probes", bytes.NewReader(body))
		input.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server := &Server{config: ServerConfig{PeerProbes: reportingProbe{PeerProbeResult{Path: path, ConnectionNanoseconds: 42}}}}
		server.peerProbe(response, input, localRequestID(), Peer{})
		var result PeerProbeResult
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Path != path || result.ConnectionNanoseconds != 42 {
			t.Fatalf("path=%q status=%d result=%#v", path, response.Code, result)
		}
	}
}

func TestProbeErrorPreservesAuthorityAndDeadline(t *testing.T) {
	for _, cause := range []error{ErrPermission, context.DeadlineExceeded} {
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
		server := &Server{config: ServerConfig{PeerProbes: failingProbe{cause}}}
		server.peerProbe(response, input, localRequestID(), Peer{})
		err = decodeRemoteErrorReader(response.Code, response.Body)
		if !errors.Is(err, cause) {
			t.Fatalf("probe failure %v lost across IPC: status=%d error=%v", cause, response.Code, err)
		}
	}
}
