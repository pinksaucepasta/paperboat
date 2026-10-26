package runtimeattachment

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	engine := &EngineTunnels{Service: service, Forwarder: tunnelmanager.OriginStreamForwarder{Transport: &tunnelmanager.OriginHTTPTransport{}}, Prober: tunnelmanager.NetworkOriginProber{}}
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
	// The edge marks the route ready only after its readiness probe is acknowledged with one byte.
	// The probe's kind is the one the real edge sends (recorded by the tunnel module's
	// TestEngineRouteGoesReadyThroughTheRealEdgeCarrier). Round 23: unanswered, so no engine route
	// was ever observed ready.
	recorded, err := os.ReadFile("../../../../shared/runtimeattachment/testdata/engine_probe_open.json")
	if err != nil {
		t.Fatal(err)
	}
	var edgeProbe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(recorded, &edgeProbe); err != nil || edgeProbe.Kind == "" {
		t.Fatalf("recorded edge probe: %v", err)
	}
	probe, err := edge.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open := connector.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: identity.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, Generation: identity.Generation, RouteID: routeID, RequestID: "request_" + uuid.NewString(), Kind: edgeProbe.Kind}
	if err := connectorprotocol.WriteStreamOpen(probe, open); err != nil {
		t.Fatal(err)
	}
	ack := make([]byte, 1)
	if _, err := io.ReadFull(probe, ack); err != nil || ack[0] != 1 {
		t.Fatalf("readiness probe not acknowledged: %v %v", ack, err)
	}
	_ = probe.Close()
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

// PLAN 3a step 4.4: a public TCP route on the engine gets the edge's tcp_public stream with no
// decision preface and reaches its tcp origin; on a private engine tunnel a plain stream (no
// browser decision) is refused before the origin.
func TestEngineTunnelServesPublicTCPAndRefusesPlainStreamsOnPrivateTunnels(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	tcpOrigin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpOrigin.Close()
	go func() {
		for {
			c, err := tcpOrigin.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	var httpHits atomic.Int32
	httpOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { httpHits.Add(1) }))
	defer httpOrigin.Close()
	route := func(id, protocol, scheme, address string) string {
		return fmt.Sprintf(`{"id":%q,"name":%q,"protocol":%q,"match_type":"catch_all","path_prefix":null,"origin_scheme":%q,"origin_address":%q,"preserve_host":true,"host_override":null,"tls_verification":"not_applicable","tls_server_name":null,"ca_reference":null,"mtls_credential_reference":null,"connect_timeout_ms":10000,"idle_timeout_ms":90000,"max_concurrent_streams":16,"desired_state":"active"}`, id, id, protocol, scheme, address)
	}
	tunnel := func(id, access, routes string) runtimewire.EngineTunnel {
		payload := fmt.Sprintf(`{"schema":"paperboat.preview-tunnel/v1","kind":"tunnel_config_snapshot","tunnel_id":%q,"generation":1,"name":%q,"desired_state":"active","access_mode":%q,"stable_endpoint":"https://brave-blue-beacon-0421.tunnel.example.test","stable_endpoint_id":"endpoint_123e4567-e89b-42d3-a456-426614174000","expires_at":null,"routes":[%s]}`, id, id, access, routes)
		snapshot, err := hoststate.NewConfigSnapshot(id, 1, []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		return runtimewire.EngineTunnel{TunnelID: id, ConfigGeneration: 1, ConfigContentHash: snapshot.ContentHash, Snapshot: json.RawMessage(snapshot.Payload)}
	}
	list := []runtimewire.EngineTunnel{
		tunnel("tunnel_tcp", "public", route("route_tcp", "tcp", "tcp", tcpOrigin.Addr().String())),
		tunnel("tunnel_private", "private", route("route_private", "http", "http", strings.TrimPrefix(httpOrigin.URL, "http://"))+","+route("route_ptcp", "tcp_private", "tcp", tcpOrigin.Addr().String())),
	}
	tunnels := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]any{"data": map[string]any{"tunnels": list}})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
	})
	service, edge := startConnectedRuntimeCarrierWith(t, ctx, time.Now().Add(time.Hour), func() int { return http.StatusOK },
		func(a *admission) { a.EndpointsServed, a.TunnelEndpointsServed = true, true }, tunnels)
	engine := &EngineTunnels{Service: service, Forwarder: tunnelmanager.OriginStreamForwarder{Transport: &tunnelmanager.OriginHTTPTransport{}}, Prober: tunnelmanager.NetworkOriginProber{}}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	waitFor(t, ctx, func() bool { return engine.Served("tunnel_tcp") && engine.Served("tunnel_private") })
	hubs, _ := service.Hubs()
	identity := hubs[0].Identity()
	open := func(routeID, kind string) io.ReadWriteCloser {
		stream, err := edge.OpenStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		o := connector.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: identity.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, Generation: identity.Generation, RouteID: routeID, RequestID: "request_" + uuid.NewString(), Kind: kind}
		if err := connectorprotocol.WriteStreamOpen(stream, o); err != nil {
			t.Fatal(err)
		}
		return stream
	}
	tcp := open("route_tcp", "tcp_public")
	if _, err := io.WriteString(tcp, "ping"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(tcp, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("public TCP through the engine: %q %v", echo, err)
	}
	_ = tcp.Close()
	// A private TCP stream (opened by the edge for an authorized accessor grant) reaches its origin.
	private := open("route_ptcp", "tcp_private")
	if _, err := io.WriteString(private, "pong"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(private, echo); err != nil || string(echo) != "pong" {
		t.Fatalf("private TCP through the engine: %q %v", echo, err)
	}
	_ = private.Close()
	plain := open("route_private", "https")
	_, _ = io.WriteString(plain, "GET / HTTP/1.1\r\nHost: brave-blue-beacon-0421.tunnel.example.test\r\nConnection: close\r\n\r\n")
	if response, err := http.ReadResponse(bufio.NewReader(plain), nil); err == nil {
		response.Body.Close()
		t.Fatalf("a plain stream on a private engine tunnel got %d", response.StatusCode)
	}
	if httpHits.Load() != 0 {
		t.Fatal("a plain stream on a private engine tunnel reached the origin")
	}
}

// PLAN 3a step 5: the agent probes each engine tunnel route's local app and reports a stopped one
// as unreachable on the server's origin-health path, so `pb tunnel status` can name it.
func TestEngineTunnelReportsAStoppedOriginAsUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	stopped, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := stopped.Addr().String()
	_ = stopped.Close() // nothing listens: every probe is refused
	const tunnelID, routeID = "tunnel_down", "route_down"
	payload := fmt.Sprintf(`{"schema":"paperboat.preview-tunnel/v1","kind":"tunnel_config_snapshot","tunnel_id":%q,"generation":1,"name":"down","desired_state":"active","access_mode":"public","stable_endpoint":"https://brave-blue-beacon-0421.tunnel.example.test","stable_endpoint_id":"endpoint_123e4567-e89b-42d3-a456-426614174000","expires_at":null,"routes":[{"id":%q,"name":"default","protocol":"http","match_type":"catch_all","path_prefix":null,"origin_scheme":"http","origin_address":%q,"preserve_host":true,"host_override":null,"tls_verification":"not_applicable","tls_server_name":null,"ca_reference":null,"mtls_credential_reference":null,"connect_timeout_ms":1000,"idle_timeout_ms":90000,"max_concurrent_streams":16,"desired_state":"active"}]}`, tunnelID, routeID, address)
	snapshot, err := hoststate.NewConfigSnapshot(tunnelID, 1, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	reports := make(chan runtimewire.OriginHealthRequest, 4)
	server := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case runtimewire.HostTunnelsPath:
			list := []runtimewire.EngineTunnel{{TunnelID: tunnelID, ConfigGeneration: 1, ConfigContentHash: snapshot.ContentHash, Snapshot: json.RawMessage(snapshot.Payload)}}
			body, _ := json.Marshal(map[string]any{"data": map[string]any{"tunnels": list}})
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
		case runtimewire.HostTunnelOriginHealthPath:
			var report runtimewire.OriginHealthRequest
			_ = json.NewDecoder(request.Body).Decode(&report)
			reports <- report
			return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("{}")), Request: request}, nil
	})
	service, _ := startConnectedRuntimeCarrierWith(t, ctx, time.Now().Add(time.Hour), func() int { return http.StatusOK },
		func(a *admission) { a.EndpointsServed, a.TunnelEndpointsServed = true, true }, server)
	engine := &EngineTunnels{Service: service, Forwarder: tunnelmanager.OriginStreamForwarder{Transport: &tunnelmanager.OriginHTTPTransport{}}, Prober: tunnelmanager.NetworkOriginProber{}, originInterval: 20 * time.Millisecond}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	select {
	case report := <-reports:
		if len(report.Routes) != 1 || report.Routes[0].TunnelID != tunnelID || report.Routes[0].RouteID != routeID || report.Routes[0].State != "unreachable" || report.Routes[0].Code != "refused" {
			t.Fatalf("origin health report = %+v", report)
		}
	case <-ctx.Done():
		t.Fatal("a stopped origin was never reported")
	}
}

// dev-099c1c94 (2026-10-26): the daemon built EngineTunnels without an origin transport, so every
// engine route was refused and no tunnel was served; Start refuses that configuration.
func TestEngineTunnelsRefuseToStartWithoutAnOriginTransport(t *testing.T) {
	engine := &EngineTunnels{Service: &Service{}}
	if err := engine.Start(t.Context()); err == nil {
		_ = engine.Shutdown(context.Background())
		t.Fatal("engine tunnels started without an origin transport")
	}
}
