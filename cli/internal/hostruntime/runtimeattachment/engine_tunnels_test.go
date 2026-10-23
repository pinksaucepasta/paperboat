package runtimeattachment

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

// PLAN 3a step 4.3: once every edge serves this agent's tunnels, the agent serves each engine
// tunnel's routes on its runtime carrier: a stream the edge opens for the route on that carrier
// reaches the route's origin, and a tunnel the server takes off the engine stops being served.
func TestEngineTunnelRouteRidesOnTheAgentRuntimeCarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "engine-ok") }))
	defer origin.Close()
	const tunnelID, routeID = "tunnel_engine", "route_engine"
	payload := fmt.Sprintf(`{"schema":"paperboat.preview-tunnel/v1","kind":"tunnel_config_snapshot","tunnel_id":%q,"generation":1,"name":"engine","desired_state":"active","access_mode":"public","stable_endpoint":"https://brave-blue-beacon-0421.tunnel.example.test","stable_endpoint_id":"endpoint_123e4567-e89b-42d3-a456-426614174000","expires_at":null,"routes":[{"id":%q,"name":"default","protocol":"http","match_type":"catch_all","path_prefix":null,"origin_scheme":"http","origin_address":%q,"preserve_host":true,"host_override":null,"tls_verification":"not_applicable","tls_server_name":null,"ca_reference":null,"mtls_credential_reference":null,"connect_timeout_ms":10000,"idle_timeout_ms":90000,"max_concurrent_streams":16,"desired_state":"active"}]}`, tunnelID, routeID, strings.TrimPrefix(origin.URL, "http://"))
	snapshot, err := hoststate.NewConfigSnapshot(tunnelID, 1, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	var listed atomic.Bool
	listed.Store(true)
	var fetchCount atomic.Int32
	tunnels := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != runtimewire.HostTunnelsPath {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("{}")), Request: request}, nil
		}
		fetchCount.Add(1)
		list := []runtimewire.EngineTunnel{}
		if listed.Load() {
			list = append(list, runtimewire.EngineTunnel{TunnelID: tunnelID, ConfigGeneration: 1, ConfigContentHash: snapshot.ContentHash, Snapshot: json.RawMessage(snapshot.Payload)})
		}
		body, _ := json.Marshal(map[string]any{"data": map[string]any{"tunnels": list}})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
	})
	service, edge := startConnectedRuntimeCarrierWith(t, ctx, time.Now().Add(time.Hour), func() int { return http.StatusOK },
		func(a *admission) { a.EndpointsServed, a.TunnelEndpointsServed = true, true }, tunnels)
	engine := &EngineTunnels{Service: service, Forwarder: tunnelmanager.OriginStreamForwarder{Transport: &tunnelmanager.OriginHTTPTransport{}}}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	waitFor(t, ctx, func() bool { return engine.Served(tunnelID) })
	hubs, _ := service.Hubs()
	if len(hubs) != 1 {
		t.Fatalf("hubs = %d", len(hubs))
	}
	identity := hubs[0].Identity()
	request := func() (string, error) {
		stream, err := edge.OpenStream(ctx)
		if err != nil {
			return "", err
		}
		defer stream.Close()
		open := connector.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: identity.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, Generation: identity.Generation, RouteID: routeID, RequestID: "request_" + uuid.NewString(), Kind: "https"}
		if err := connectorprotocol.WriteStreamOpen(stream, open); err != nil {
			return "", err
		}
		if _, err := io.WriteString(stream, "GET / HTTP/1.1\r\nHost: brave-blue-beacon-0421.tunnel.example.test\r\nConnection: close\r\n\r\n"); err != nil {
			return "", err
		}
		response, err := http.ReadResponse(bufio.NewReader(stream), nil)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return string(body), err
	}
	if body, err := request(); err != nil || body != "engine-ok" {
		t.Fatalf("engine route through the runtime carrier: %q %v", body, err)
	}
	// The server takes the tunnel off the engine (served flag unchanged, list empty): after the next
	// fetch the route is not served and Served reports false.
	listed.Store(false)
	before := fetchCount.Load()
	engine.refresh(ctx, true)
	engine.reconcile(ctx, hubs)
	if fetchCount.Load() == before || engine.Served(tunnelID) {
		t.Fatal("tunnel still served after leaving the engine list")
	}
	if body, err := request(); err == nil && body == "engine-ok" {
		t.Fatal("route still forwarded after its tunnel left the engine")
	}
}

func waitFor(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("condition not reached")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
