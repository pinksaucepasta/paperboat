package tunnelmanager

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

func TestIngressForwarderDeniesBeforeOriginAndExpiresActiveBody(t *testing.T) {
	testIngressForwarderAuthority(t, time.Second, false)
}
func TestIngressForwarderRevocationClosesActiveOrigin(t *testing.T) {
	testIngressForwarderAuthority(t, 10*time.Second, true)
}
func testIngressForwarderAuthority(t *testing.T, lifetime time.Duration, revoke bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var hits atomic.Int32
	var revoked atomic.Bool
	started, stopped := make(chan struct{}, 1), make(chan struct{}, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
		stopped <- struct{}{}
	}))
	defer origin.Close()
	identity := connector.DataCarrierIdentity{AccountID: "account-1", HostID: "host-1", TunnelID: "tunnel-1", ConnectorID: "connector-1", SessionID: "session-1", ProcessGeneration: 1, Generation: 1}
	config := connector.DefaultDataCarrierPoolConfig()
	config.MaximumCarriers = 1
	config.Session = identity
	config.FailureDomains = []string{"edge-1"}
	config.Targets = []connector.DataCarrierTarget{{EdgeID: "edge-1", ProcessEpoch: "epoch-1234567890123456789012345678", FailureDomain: "edge-1"}}
	var edge *connector.DataCarrier
	prepared, err := connector.PrepareDataCarrier(ctx, identity, config, func(_ context.Context, r connector.DataCarrierDialRequest) (connector.DataCarrierDialResult, error) {
		a, b := net.Pipe()
		var err error
		edge, err = connector.NewDataCarrierServer(ctx, b, config.Carrier, connector.DataCarrierAdmission{Identity: identity, Authorize: func(context.Context, connector.StreamOpen) error { return nil }})
		return connector.DataCarrierDialResult{Link: a, PeerIdentity: identity, Transport: r.Transport, EdgeID: r.EdgeID, FailureDomain: r.FailureDomain}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := prepared.Activate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close(context.Background())
	defer edge.Close()
	route := hoststate.TunnelConfigRoute{ID: "route-1", Protocol: "http", OriginScheme: "http", OriginAddress: origin.Listener.Addr().String(), TLSVerification: "not_applicable", PreserveHost: true, ConnectTimeoutMs: 1000, IdleTimeoutMs: 2000, MaxConcurrentStreams: 2, DesiredState: "active"}
	now := time.Now().UTC()
	d := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "env-1", AccountID: identity.AccountID, TunnelID: identity.TunnelID, Lifecycle: "durable", ResourceGeneration: 1, RouteID: route.ID, RouteGeneration: 1, TargetID: route.ID, TargetGeneration: 1, HostID: identity.HostID, InstallationGeneration: 1, Audience: "public", ConnectionMethod: "edge", Protocol: "http", Hostname: "app.example.test", PathPrefix: "/", OriginScheme: "http", OriginAddress: route.OriginAddress, TLSVerification: route.TLSVerification, PublicationID: route.ID, PublicationGeneration: 1}, DecisionID: "decision-1", PolicyGeneration: 1, EdgeNodeID: "edge-1", EdgeProcessEpoch: "epoch-1234567890123456789012345678", ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: 1, ConfigGeneration: 1, AssignmentGeneration: 1, Action: "view", IssuedAt: now, ExpiresAt: now.Add(lifetime)}
	f := OriginStreamForwarder{Transport: &OriginHTTPTransport{}, IngressAuthority: func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		if revoked.Load() {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}
		return d, nil
	}}
	running, err := f.Start(ctx, active, []hoststate.TunnelConfigRoute{route})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close(ctx)
	open := func(decision connectorprotocol.IngressDecision, id string) io.ReadWriteCloser {
		s, err := edge.OpenStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		preface := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: identity.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: 1, Generation: 1, RouteID: route.ID, RequestID: id, Kind: "http"}
		if err := connectorprotocol.WriteStreamOpen(s, preface); err != nil {
			t.Fatal(err)
		}
		if err := connectorprotocol.WriteIngressDecision(s, decision, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		return s
	}
	wrong := d
	wrong.Binding.OriginAddress = "127.0.0.1:1"
	s := open(wrong, "bad-target")
	_, _ = io.WriteString(s, "GET / HTTP/1.1\r\nHost: app.example.test\r\n\r\n")
	_, err = io.ReadAll(s)
	_ = s.Close()
	if hits.Load() != 0 {
		t.Fatal("wrong target dialed origin")
	}
	wrongEdge := d
	wrongEdge.EdgeNodeID = "edge-other"
	s = open(wrongEdge, "wrong-edge")
	_, _ = io.WriteString(s, "GET / HTTP/1.1\r\nHost: app.example.test\r\n\r\n")
	_, _ = io.ReadAll(s)
	_ = s.Close()
	if hits.Load() != 0 {
		t.Fatal("wrong carrier identity reached origin")
	}
	s = open(d, "good-request")
	defer s.Close()
	_, err = io.WriteString(s, "GET / HTTP/1.1\r\nHost: app.example.test\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("origin did not start")
	}
	if revoke {
		revoked.Store(true)
	}
	go func() {
		response, err := http.ReadResponse(bufio.NewReader(s), nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("withdrawn or expired authority did not cancel active origin")
	}
	if hits.Load() != 1 {
		t.Fatalf("origin hits=%d", hits.Load())
	}
}
