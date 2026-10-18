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

// Device finding: a 16 MiB HTTP/2 POST through a durable tunnel was cut after
// about 4 MB (about 10s).
func TestIngressForwarderCarriesALongUploadPastTheDecisionLifetime(t *testing.T) {
	const lifetime = 10 * time.Second
	const revoke = false
	if testing.Short() {
		t.Skip("14s real-time upload")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var received atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		received.Store(n)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
	s := open(d, "long-upload")
	defer s.Close()
	const total, chunk = 16 << 20, 64 << 10
	if _, err := io.WriteString(s, "POST /upload HTTP/1.1\r\nHost: app.example.test\r\nContent-Length: 16777216\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// About 13s of upload, longer than the 10s ingress decision lifetime.
	payload := make([]byte, chunk)
	for sent := 0; sent < total; sent += chunk {
		if _, err := s.Write(payload); err != nil {
			t.Fatalf("upload cut after %d bytes: %v", sent, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	response, err := http.ReadResponse(bufio.NewReader(s), nil)
	if err != nil || response.StatusCode != http.StatusNoContent || received.Load() != total {
		t.Fatalf("response=%v err=%v origin received %d of %d bytes", response, err, received.Load(), total)
	}
}
