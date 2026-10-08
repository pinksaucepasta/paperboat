package tunnelmanager

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

type task24Descriptor struct {
	Protocol, Address, CACert, ClientCert, ClientKey, ReadyPath, StopPath, OriginsPath, AuthorityPath, ObservationsPath string
	Nodes                                                                                                               []task30CarrierNode
	DockerSimulation                                                                                                    bool
	HTTPHostname                                                                                                        string
	FixtureTimeoutSeconds                                                                                               int
}
type task30CarrierNode struct{ Address, NodeID, Epoch, Domain string }

type task24Origins struct{ HTTP, TCP string }
type task24Authority struct {
	HTTP, TCP connectorprotocol.IngressDecision
	Nodes     []task24Authority
}

func TestTask24CrossRepositoryDaemon(t *testing.T) {
	path := os.Getenv("PAPERBOAT_TASK24_DESCRIPTOR")
	if path == "" {
		t.Skip("run by the edge Task24 interop coordinator")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var descriptor task24Descriptor
	if err := json.Unmarshal(contents, &descriptor); err != nil {
		t.Fatal(err)
	}
	if descriptor.DockerSimulation && descriptor.ObservationsPath == "" {
		t.Fatal("Docker simulation observations path is required")
	}
	var observationsMu sync.Mutex
	recordObservation := func(id string) error {
		observationsMu.Lock()
		defer observationsMu.Unlock()
		encoded, err := json.Marshal(id)
		if err != nil {
			return err
		}
		file, err := os.OpenFile(descriptor.ObservationsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = file.Write(append(encoded, '\n'))
		return err
	}
	certificate, err := tls.X509KeyPair([]byte(descriptor.ClientCert), []byte(descriptor.ClientKey))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(descriptor.CACert)) {
		t.Fatal("invalid fixture CA")
	}
	clientTLS := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: roots, ServerName: "localhost"}
	identity := connector.DataCarrierIdentity{AccountID: "account_replica", HostID: "host_pinned", TunnelID: "tunnel_replica", ConnectorID: "connector_pinned", SessionID: "session_pinned", ProcessGeneration: 3, Generation: 3}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := ""
		if descriptor.DockerSimulation {
			requestID = strings.TrimSpace(r.Header.Get("X-Task30-ID"))
			if requestID == "" {
				http.Error(w, "missing simulation request ID", http.StatusBadRequest)
				return
			}
			if !strings.HasPrefix(requestID, "held-") {
				if err := recordObservation(requestID); err != nil {
					http.Error(w, "record simulation observation", http.StatusInternalServerError)
					return
				}
			}
		}
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			connection, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer connection.Close()
			_, _ = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nearly")
			_ = buffer.Flush()
			late := make([]byte, 4)
			if _, err = io.ReadFull(connection, late); err == nil {
				_, _ = connection.Write(append([]byte("echo:"), late...))
			}
			return
		}
		if descriptor.DockerSimulation && r.Method == http.MethodGet && r.URL.Path == "/browser" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><title>Paperboat Task30</title><body>paperboat-task30-browser-ready</body>"))
			return
		}
		if r.URL.Path != "/stream" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "task24-upload" {
			http.Error(w, "invalid upload", http.StatusBadRequest)
			return
		}
		if descriptor.DockerSimulation && strings.HasPrefix(requestID, "held-") {
			if err := recordObservation(requestID); err != nil {
				http.Error(w, "record simulation observation", http.StatusInternalServerError)
				return
			}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("task24-origin-ok"))
	}))
	defer origin.Close()
	tcpOrigin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpOrigin.Close()
	fixtureTimeout := 20 * time.Second
	if descriptor.DockerSimulation {
		fixtureTimeout = 120 * time.Second
		if descriptor.FixtureTimeoutSeconds >= 120 && descriptor.FixtureTimeoutSeconds <= 900 {
			fixtureTimeout = time.Duration(descriptor.FixtureTimeoutSeconds) * time.Second
		} else if descriptor.FixtureTimeoutSeconds != 0 {
			t.Fatal("Docker simulation fixture timeout must be between 120 and 900 seconds")
		}
	} else if descriptor.FixtureTimeoutSeconds != 0 {
		t.Fatal("fixture timeout override requires Docker simulation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	if descriptor.DockerSimulation {
		go func() {
			<-ctx.Done()
			_ = tcpOrigin.Close()
		}()
	}
	go func() {
		for accepted := 0; descriptor.DockerSimulation || accepted < 2; accepted++ {
			connection, err := tcpOrigin.Accept()
			if err != nil {
				return
			}
			upload, _ := io.ReadAll(connection)
			validUpload := string(upload) == "tcp-upload"
			if descriptor.DockerSimulation {
				requestID, found := strings.CutPrefix(string(upload), "tcp-upload:")
				validUpload = found && requestID != "" && recordObservation(requestID) == nil
			}
			if validUpload {
				_, _ = connection.Write([]byte("tcp-delayed-reply"))
			} else {
				_, _ = connection.Write([]byte("application-auth-rejected"))
			}
			if half, ok := connection.(interface{ CloseWrite() error }); ok {
				_ = half.CloseWrite()
			}
			_ = connection.Close()
		}
	}()
	originsJSON, _ := json.Marshal(task24Origins{HTTP: origin.Listener.Addr().String(), TCP: tcpOrigin.Addr().String()})
	if err := os.WriteFile(descriptor.OriginsPath, originsJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	var authority task24Authority
	for {
		contents, readErr := os.ReadFile(descriptor.AuthorityPath)
		if readErr == nil {
			if json.Unmarshal(contents, &authority) != nil {
				t.Fatal("invalid authority fixture")
			}
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	endpoint := connector.DataCarrierEndpointConfig{Address: descriptor.Address, TLS: clientTLS, ExpectedIdentity: identity, PeerBinding: func(state tls.ConnectionState) (connector.DataCarrierIdentity, error) {
		if len(state.PeerCertificates) == 0 {
			return connector.DataCarrierIdentity{}, errors.New("missing edge certificate")
		}
		return identity, nil
	}}
	poolConfig := connector.DefaultDataCarrierPoolConfig()
	poolConfig.Targets = []connector.DataCarrierTarget{{EdgeID: "edge_01", ProcessEpoch: "epoch_0001", FailureDomain: "domain-a"}}
	poolConfig.MaximumCarriers = 1
	poolConfig.FailureDomains = []string{"task24-edge"}
	poolConfig.Session = identity
	poolConfig.SingleTransport = true
	if descriptor.Protocol == "http3" {
		poolConfig.Preferred = connector.HTTP3
		poolConfig.Fallback = connector.HTTP3
	} else {
		poolConfig.Preferred = connector.HTTP2
		poolConfig.Fallback = connector.HTTP2
	}
	dialer := connector.NewHTTPNetworkDialer(connector.NetworkDialerConfig{TCPMux: endpoint, QUIC: endpoint})
	if len(descriptor.Nodes) > 0 {
		poolConfig.MaximumCarriers = len(descriptor.Nodes)
		poolConfig.Targets = nil
		byNode := map[string]connector.DataCarrierEndpointConfig{}
		for _, node := range descriptor.Nodes {
			poolConfig.Targets = append(poolConfig.Targets, connector.DataCarrierTarget{EdgeID: node.NodeID, ProcessEpoch: node.Epoch, FailureDomain: node.Domain})
			next := endpoint
			next.Address = node.Address
			byNode[node.NodeID] = next
		}
		dialer = func(ctx context.Context, r connector.DataCarrierDialRequest) (connector.DataCarrierDialResult, error) {
			e, ok := byNode[r.EdgeID]
			if !ok {
				return connector.DataCarrierDialResult{}, connector.ErrInvalidDataCarrierEndpoint
			}
			return connector.NewHTTPNetworkDialer(connector.NetworkDialerConfig{TCPMux: e, QUIC: e})(ctx, r)
		}
	}
	prepared, err := connector.PrepareDataCarrier(ctx, identity, poolConfig, dialer)
	if err != nil {
		t.Fatal(err)
	}
	active, err := prepared.Activate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close(context.Background())
	httpHostname := descriptor.HTTPHostname
	if httpHostname == "" {
		httpHostname = "app.customer.test"
	}
	httpRoute := hoststate.TunnelConfigRoute{ID: "route_http", Name: "task24-http", Protocol: "http", MatchType: "exact", MatchHostname: httpHostname, OriginScheme: "http", OriginAddress: origin.Listener.Addr().String(), PreserveHost: true, TLSVerification: "not_applicable", ConnectTimeoutMs: 1000, IdleTimeoutMs: 2000, MaxConcurrentStreams: 4, DesiredState: "active"}
	tcpRoute := hoststate.TunnelConfigRoute{ID: "route_tcp", Name: "task24-tcp", Protocol: "tcp", OriginScheme: "tcp", OriginAddress: tcpOrigin.Addr().String(), TLSVerification: "not_applicable", ConnectTimeoutMs: 1000, IdleTimeoutMs: 2000, MaxConcurrentStreams: 2, DesiredState: "active"}
	ingressAuthority := func(_ context.Context, _ connectorprotocol.StreamOpen, candidate connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		for _, node := range authority.Nodes {
			if node.HTTP.EdgeNodeID == candidate.EdgeNodeID && node.HTTP.EdgeProcessEpoch == candidate.EdgeProcessEpoch {
				if candidate.Binding.RouteID == node.HTTP.Binding.RouteID {
					return refreshTask30SimulationAuthority(node.HTTP, descriptor.DockerSimulation), nil
				}
				if candidate.Binding.RouteID == node.TCP.Binding.RouteID {
					return refreshTask30SimulationAuthority(node.TCP, descriptor.DockerSimulation), nil
				}
			}
		}
		if candidate.Binding.RouteID == authority.HTTP.Binding.RouteID {
			return refreshTask30SimulationAuthority(authority.HTTP, descriptor.DockerSimulation), nil
		}
		if candidate.Binding.RouteID == authority.TCP.Binding.RouteID {
			return refreshTask30SimulationAuthority(authority.TCP, descriptor.DockerSimulation), nil
		}
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	running, err := (OriginStreamForwarder{Transport: &OriginHTTPTransport{}, IngressAuthority: ingressAuthority}).Start(ctx, active, []hoststate.TunnelConfigRoute{httpRoute, tcpRoute})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close(context.Background())
	if err := os.WriteFile(descriptor.ReadyPath, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(descriptor.StopPath); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func refreshTask30SimulationAuthority(decision connectorprotocol.IngressDecision, simulation bool) connectorprotocol.IngressDecision {
	if simulation {
		decision.IssuedAt = time.Now().UTC()
		decision.ExpiresAt = decision.IssuedAt.Add(10 * time.Second)
	}
	return decision
}
