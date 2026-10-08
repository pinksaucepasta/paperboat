//go:build darwin || linux

package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type failingProbe struct{ err error }

func (p failingProbe) ProbePeer(context.Context, Peer, PeerStreamRequest) (PeerProbeResult, error) {
	return PeerProbeResult{}, p.err
}

type reportingProbe struct{ result PeerProbeResult }

type privateProbeError struct{}

func (privateProbeError) Error() string { panic("private error text must never be formatted") }
func (privateProbeError) Unwrap() error { return syscall.ECONNREFUSED }

func TestProbeFailureKeepsCausePrivateAndCorrelated(t *testing.T) {
	value, err := NewPeerStreamRequest("machine_test", "environment_test", 1, "health_probe", "operation_test", "credential_test", time.Now().Add(time.Minute), 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	reference := supportref.New()
	input := httptest.NewRequest(http.MethodPost, "/v1/peer-probes", bytes.NewReader(body))
	input = input.WithContext(supportref.WithContext(input.Context(), reference))
	input.Header.Set("Content-Type", "application/json")
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	defer restore()
	response := httptest.NewRecorder()
	server := &Server{config: ServerConfig{PeerProbes: failingProbe{privateProbeError{}}}}
	server.peerProbe(response, input, localRequestID(), Peer{})
	remote, ok := decodeRemoteErrorReader(response.Code, response.Body).(*RemoteError)
	if !ok || response.Code != http.StatusServiceUnavailable || remote.Code != "peer_probe_unavailable" || !strings.Contains(remote.Message, "pb doctor") {
		t.Fatal("probe failure did not produce its actionable envelope")
	}
	if len(faults) != 1 || faults[0].Cause != "connection_refused" || faults[0].Stage != "peer_connect" || faults[0].SupportReference != reference {
		t.Fatalf("probe fault=%#v", faults)
	}
}

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

func TestProbeMixedPermissionFailureIsOperational(t *testing.T) {
	value, err := NewPeerStreamRequest("machine_test", "environment_test", 1, "health_probe", "operation_test", "credential_test", time.Now().Add(time.Minute), 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, proven := range []error{ErrPermission, PermissionFailure(errors.New("private proof"))} {
		input := httptest.NewRequest(http.MethodPost, "/v1/peer-probes", bytes.NewReader(body))
		input.Header.Set("Content-Type", "application/json")
		var faults []errorreport.Fault
		restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
		response := httptest.NewRecorder()
		server := &Server{config: ServerConfig{PeerProbes: failingProbe{errors.Join(proven, syscall.EIO)}}}
		server.peerProbe(response, input, localRequestID(), Peer{})
		restore()
		if response.Code != http.StatusServiceUnavailable || len(faults) != 1 || faults[0].Errno != int(syscall.EIO) {
			t.Fatal("operational sibling was hidden by permission denial")
		}
	}
}
