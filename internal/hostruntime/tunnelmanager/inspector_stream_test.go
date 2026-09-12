package tunnelmanager

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

type authenticatedInspectorStub struct{ calls int }

func (s *authenticatedInspectorStub) ServeAuthenticatedHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls++
	if r.Header.Get("X-Paperboat-Inspector-Grant") != "iat_test" {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"records":[]}`)
}

func TestServeInspectorStreamDispatchesOnlyInspectorHTTP(t *testing.T) {
	edge, daemon := net.Pipe()
	defer edge.Close()
	defer daemon.Close()
	service := &authenticatedInspectorStub{}
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: "account_1", TunnelID: "tunnel_1", ConnectorID: "connector_1", SessionID: "session_1", ProcessGeneration: 1, Generation: 1, RouteID: "route_1", RequestID: "request_1", Kind: connectorprotocol.InspectorHTTP}
	done := make(chan error, 1)
	go func() {
		err := ServeInspectorStream(context.Background(), daemon, open, service)
		_ = daemon.Close()
		done <- err
	}()
	request, _ := http.NewRequest(http.MethodGet, "http://paperboatd.local/v1/inspector/records?kind=tunnel&resource=tunnel_1&route=route_1", nil)
	request.Header.Set("X-Paperboat-Inspector-Grant", "iat_test")
	if err := request.Write(edge); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(edge), request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || service.calls != 1 {
		t.Fatalf("status=%d calls=%d", response.StatusCode, service.calls)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	bad := open
	bad.Kind = "http"
	if err := ServeInspectorStream(context.Background(), nilStream{}, bad, service); err != ErrInspectorStreamInvalid {
		t.Fatalf("wrong-kind error=%v", err)
	}
}

type nilStream struct{}

func (nilStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (nilStream) Write(p []byte) (int, error) { return len(p), nil }
func (nilStream) Close() error                { return nil }
