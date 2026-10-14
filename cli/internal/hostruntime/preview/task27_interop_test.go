package preview

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

type task27Descriptor struct {
	Address, CACert, ClientCert, ClientKey string
	AuthorityURL, AuthorityCA              string
	ReadyPath, StopPath                    string
	MachineID, AccountID, HostID           string
	TunnelID, ConnectorID, SessionID       string
	EdgeNodeID, EdgeProcessEpoch           string
	BootID                                 string
	InstallationGeneration                 int64
}

type task27Ready struct {
	DispatchURL            string `json:"dispatch_url"`
	OriginAddress          string `json:"origin_address"`
	BootID                 string `json:"boot_id"`
	InstallationGeneration int64  `json:"installation_generation"`
}

type task27State struct {
	State      string `json:"state"`
	Generation int64  `json:"generation"`
	OriginHits int64  `json:"origin_hits"`
	Stopped    bool   `json:"stopped"`
}

type task27LeaseClient struct{ stopped atomic.Bool }

func (*task27LeaseClient) Create(context.Context, LeaseRequest) (Lease, error) {
	return Lease{}, ErrSessionInvalid
}
func (*task27LeaseClient) Renew(_ context.Context, lease Lease, _ string) (Lease, error) {
	lease.Generation++
	lease.ETag = formatLeaseETag(lease.ID, lease.Generation)
	lease.LastRenewedAt = time.Now().UTC()
	return lease, nil
}
func (c *task27LeaseClient) Stop(context.Context, Lease, string) error {
	c.stopped.Store(true)
	return nil
}

type task27Readiness struct {
	mu    sync.Mutex
	state task27State
}

func (o *task27Readiness) ObservePreviewReadiness(_ context.Context, _ DispatchReadiness, lease Lease, expected int64) (Lease, error) {
	lease = sessionReadyLease(lease)
	lease.Generation, lease.ETag = expected+1, formatLeaseETag(lease.ID, expected+1)
	o.mu.Lock()
	o.state.State, o.state.Generation = "ready", lease.Generation
	o.mu.Unlock()
	return lease, nil
}

type task27CarrierResolver struct {
	live      *connector.ActiveDataCarrier
	identity  connector.DataCarrierIdentity
	authority tunnelmanager.IngressAuthorityFunc
}

func (r task27CarrierResolver) ResolvePreviewCarrier(_ context.Context, request DispatchRequest) (Carrier, error) {
	return NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Active: r.live, Identity: r.identity, RouteID: request.PreviewID, BrowserIngress: r.authority, BrowserRouteGeneration: uint64(request.ExpectedGeneration), OriginDialTimeout: LazyOriginConnectTimeout})
}

// TestTask27LazyDaemonInterop is an explicit cross-repository test seam. The
// coordinator supplies the authenticated edge carrier and signed dispatch;
// this helper runs the real daemon DispatchManager, SessionManager, carrier,
// exact loopback origin, readiness transition, and cleanup.
func TestTask27LazyDaemonInterop(t *testing.T) {
	path := os.Getenv("PAPERBOAT_TASK27_DESCRIPTOR")
	if path == "" {
		t.Skip("invoked by the Task27 activation coordinator")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d task27Descriptor
	if json.Unmarshal(raw, &d) != nil || d.ReadyPath == "" || d.StopPath == "" || d.BootID == "" || d.EdgeNodeID == "" || d.EdgeProcessEpoch == "" || d.InstallationGeneration < 1 {
		t.Fatal("invalid Task27 descriptor")
	}
	certificate, err := tls.X509KeyPair([]byte(d.ClientCert), []byte(d.ClientKey))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(d.CACert)) {
		t.Fatal("invalid carrier CA")
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
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	identity := connector.DataCarrierIdentity{AccountID: d.AccountID, HostID: d.HostID, TunnelID: d.TunnelID, ConnectorID: d.ConnectorID, SessionID: d.SessionID, ProcessGeneration: 1, Generation: 1}
	endpoint := connector.DataCarrierEndpointConfig{Address: d.Address, TLS: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: roots, ServerName: "localhost"}, ExpectedIdentity: identity, PeerBinding: func(state tls.ConnectionState) (connector.DataCarrierIdentity, error) {
		if len(state.PeerCertificates) == 0 {
			return connector.DataCarrierIdentity{}, errors.New("missing edge certificate")
		}
		return identity, nil
	}}
	pool := connector.DefaultDataCarrierPoolConfig()
	pool.MaximumCarriers = 1
	pool.FailureDomains = []string{"task27-edge"}
	pool.Targets = []connector.DataCarrierTarget{{EdgeID: d.EdgeNodeID, ProcessEpoch: d.EdgeProcessEpoch, FailureDomain: "task27-edge"}}
	pool.Session = identity
	pool.SingleTransport = true
	pool.Preferred, pool.Fallback = connector.HTTP2, connector.HTTP2
	prepared, err := connector.PrepareDataCarrier(ctx, identity, pool, connector.NewHTTPNetworkDialer(connector.NetworkDialerConfig{TCPMux: endpoint}))
	if err != nil {
		t.Fatal(err)
	}
	live, err := prepared.Activate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close(context.Background())
	var originHits atomic.Int64
	var originMu sync.Mutex
	var originServer *http.Server
	var originListener net.Listener
	startOrigin := func(address, body string) error {
		listener, listenErr := net.Listen("tcp", address)
		if listenErr != nil {
			return listenErr
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodHead {
				originHits.Add(1)
			}
			_, _ = io.WriteString(w, body)
		})}
		originServer, originListener = server, listener
		go func() { _ = server.Serve(listener) }()
		return nil
	}
	if err := startOrigin("127.0.0.1:0", "task27-origin-v1"); err != nil {
		t.Fatal(err)
	}
	originAddress := originListener.Addr().String()
	defer func() {
		originMu.Lock()
		if originServer != nil {
			_ = originServer.Close()
		}
		originMu.Unlock()
	}()
	readiness := &task27Readiness{}
	leases := &task27LeaseClient{}
	manager, err := NewDispatchManager(DispatchManagerConfig{MachineID: d.MachineID, InstallationGeneration: d.InstallationGeneration, BootID: d.BootID, Leases: leases, Carriers: task27CarrierResolver{live: live, identity: identity, authority: authority}, Readiness: readiness, Owners: dispatchOwners{done: make(chan struct{})}, RunContext: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	mux := http.NewServeMux()
	mux.HandleFunc("/dispatch", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		var request DispatchRequest
		if r.Method != http.MethodPost || decoder.Decode(&request) != nil {
			http.Error(w, "invalid dispatch", http.StatusBadRequest)
			return
		}
		authorization := testDispatchAuthorization(request, time.Now().UTC())
		outcome, err := manager.Dispatch(r.Context(), authorization, request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(outcome)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		readiness.mu.Lock()
		state := readiness.state
		readiness.mu.Unlock()
		state.OriginHits, state.Stopped = originHits.Load(), leases.stopped.Load()
		_ = json.NewEncoder(w).Encode(state)
	})
	mux.HandleFunc("/replace", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		originMu.Lock()
		previous := originServer
		if previous == nil {
			originMu.Unlock()
			http.Error(w, "origin unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := previous.Close(); err != nil {
			originMu.Unlock()
			http.Error(w, "close origin", http.StatusInternalServerError)
			return
		}
		if err := startOrigin(originAddress, "task27-origin-v2"); err != nil {
			originServer, originListener = nil, nil
			originMu.Unlock()
			http.Error(w, "replace origin", http.StatusInternalServerError)
			return
		}
		originMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	dispatch := httptest.NewServer(mux)
	defer dispatch.Close()
	ready, _ := json.Marshal(task27Ready{DispatchURL: dispatch.URL, OriginAddress: originAddress, BootID: d.BootID, InstallationGeneration: d.InstallationGeneration})
	if err := os.WriteFile(d.ReadyPath, ready, 0600); err != nil {
		t.Fatal(err)
	}
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
