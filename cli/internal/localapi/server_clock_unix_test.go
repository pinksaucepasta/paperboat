//go:build darwin || linux

package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

var errBrokerReached = errors.New("broker reached")

// F21-02: a terminal or exec request carries its credential's server-clock expiry. With this machine's clock
// 10 min ahead, a 2-min credential was refused as "peer stream request is invalid" before any dial; past the
// expiry the refusal is named credential_expired.
func TestPeerStreamDeadlineIsJudgedOnTheServerClock(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	for name, clientAhead := range map[string]time.Duration{"client 10 min ahead": 10 * time.Minute, "client 10 min behind": -10 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			serverclock.Observe(time.Now().Add(-clientAhead).UTC().Format(http.TimeFormat), time.Now(), time.Now())
			reached := false
			server := &Server{config: ServerConfig{PeerStreams: peerStreamBrokerFunc(func(context.Context, Peer, PeerStreamRequest) (net.Conn, error) {
				reached = true
				return nil, errBrokerReached
			})}}
			request := PeerStreamRequest{Workspace: "personal", Schema: PeerStreamSchemaV1, MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, Consumer: "terminal", OperationID: "operation_1", Credential: "credential", MaximumBytes: 1024}
			request.Deadline = serverclock.Now().Add(2 * time.Minute).UTC()
			if response := postPeerStream(t, server, request); !reached {
				t.Fatalf("fresh credential refused before dial: %d %s", response.Code, response.Body)
			}
			reached = false
			request.Deadline = serverclock.Now().Add(-time.Second).UTC()
			response := postPeerStream(t, server, request)
			remote, ok := decodeRemoteErrorReader(response.Code, response.Body).(*RemoteError)
			if reached || !ok || remote.Code != "credential_expired" {
				t.Fatalf("expired credential: reached=%v status=%d remote=%#v", reached, response.Code, remote)
			}
		})
	}
}

func postPeerStream(t *testing.T, server *Server, request PeerStreamRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	input := httptest.NewRequest(http.MethodPost, "/v1/peer-streams", bytes.NewReader(body))
	input.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.peerStream(response, input, localRequestID(), Peer{PID: os.Getpid()})
	return response
}
