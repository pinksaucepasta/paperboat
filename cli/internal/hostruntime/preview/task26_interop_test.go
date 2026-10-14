package preview

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

type task26Descriptor struct{ Address, CACert, ClientCert, ClientKey, AuthorityURL, AuthorityCA, OriginsPath, ReadyPath, StopPath string }
type task26Origins struct{ Address, URL string }
type task26Stats struct{ Hits, Active, Stopped int32 }
type task26MachineAuth struct{}

func (task26MachineAuth) Token(context.Context) (string, error) { return "task26-machine-fixture", nil }
func (task26MachineAuth) Proof(context.Context, string, string, string, []byte) ([]byte, error) {
	return []byte("task26-proof-fixture"), nil
}

// This helper proves the cross-repository protocol and runtime integration.
// Its explicit authority fixture does not prove SQL policy or production identity.
func TestTask26CrossRepositoryBrowserDaemon(t *testing.T) {
	path := os.Getenv("PAPERBOAT_TASK26_DESCRIPTOR")
	if path == "" {
		t.Skip("invoked by the edge Task26 interop coordinator")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d task26Descriptor
	if json.Unmarshal(raw, &d) != nil {
		t.Fatal("invalid interop descriptor")
	}
	certificate, err := tls.X509KeyPair([]byte(d.ClientCert), []byte(d.ClientKey))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(d.CACert)) {
		t.Fatal("invalid fixture CA")
	}
	authRoots := x509.NewCertPool()
	if !authRoots.AppendCertsFromPEM([]byte(d.AuthorityCA)) {
		t.Fatal("invalid authority CA")
	}
	authTransport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: authRoots}}
	defer authTransport.CloseIdleConnections()
	authority, err := tunnelmanager.NewBrowserIngressAuthority(d.AuthorityURL, task26MachineAuth{}, authTransport)
	if err != nil {
		t.Fatal(err)
	}
	var hits, active, stopped atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stats" {
			_ = json.NewEncoder(w).Encode(task26Stats{hits.Load(), active.Load(), stopped.Load()})
			return
		}
		hits.Add(1)
		if r.Header.Get("Cookie") != "app=keep" {
			http.Error(w, "application cookie isolation failed", 500)
			return
		}
		if r.URL.Path == "/stream" {
			active.Add(1)
			defer active.Add(-1)
			defer stopped.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: active\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "task26-browser-origin-ok")
	}))
	defer origin.Close()
	encoded, _ := json.Marshal(task26Origins{origin.Listener.Addr().String(), origin.URL})
	if err = os.WriteFile(d.OriginsPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	identity := connector.DataCarrierIdentity{AccountID: "account_replica", HostID: "host_pinned", TunnelID: "tunnel_replica", ConnectorID: "connector_pinned", SessionID: "session_pinned", ProcessGeneration: 3, Generation: 3}
	endpoint := connector.DataCarrierEndpointConfig{Address: d.Address, TLS: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: roots, ServerName: "localhost"}, ExpectedIdentity: identity, PeerBinding: func(state tls.ConnectionState) (connector.DataCarrierIdentity, error) {
		if len(state.PeerCertificates) == 0 {
			return connector.DataCarrierIdentity{}, errors.New("missing edge certificate")
		}
		return identity, nil
	}}
	pool := connector.DefaultDataCarrierPoolConfig()
	pool.Targets = []connector.DataCarrierTarget{{EdgeID: "edge_01", ProcessEpoch: "epoch_0001", FailureDomain: "domain-a"}}
	pool.MaximumCarriers = 1
	pool.FailureDomains = []string{"task26-edge"}
	pool.Session = identity
	pool.SingleTransport = true
	pool.Preferred = connector.HTTP2
	pool.Fallback = connector.HTTP2
	prepared, err := connector.PrepareDataCarrier(ctx, identity, pool, connector.NewHTTPNetworkDialer(connector.NetworkDialerConfig{TCPMux: endpoint}))
	if err != nil {
		t.Fatal(err)
	}
	live, err := prepared.Activate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close(context.Background())
	carrier, err := NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Active: live, Identity: identity, RouteID: "route_browser", BrowserIngress: authority, BrowserRouteGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	lease := Lease{ID: "preview_browser", AccountID: identity.AccountID, Generation: 17, AccessMode: "team", Endpoint: "https://app.preview.example.test", Target: LeaseTarget{Scheme: "http", Address: origin.Listener.Addr().String()}}
	done := make(chan error, 1)
	go func() {
		done <- carrier.Run(ctx, lease, func(Lease) error { return os.WriteFile(d.ReadyPath, []byte("ready"), 0600) })
	}()
	defer func() {
		cancel()
		if err := carrier.Close(context.Background()); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("carrier shutdown did not complete")
		}
	}()
	for {
		if _, err := os.Stat(d.StopPath); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
