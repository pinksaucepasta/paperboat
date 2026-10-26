package daemoncmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"github.com/pinksaucepasta/paperboat/internal/testcert"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorSharedBrowserGatewayRoutesExactMachinesAndWithdraws(t *testing.T) {
	testCoordinatorSharedBrowserGateway(t, false, testBrowserDomain)
}

func TestCoordinatorBrowserGatewayPreservesRoutesWhenNativePortsConflict(t *testing.T) {
	testCoordinatorSharedBrowserGateway(t, true, testBrowserDomain)
}

// A web server on :80 (caddy on the Raspberry Pi test machine) made the guard
// refuse the browser gateway port, which kept the whole daemon degraded and
// blocked `pb pair`. Native routes must survive; only browser URLs go.
func TestCoordinatorBrowserGatewayPortInUseKeepsNativeRoutes(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()
	names := &coordinatorNameClient{conflictPorts: map[int]bool{80: true}}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names,
		DialMachine: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Listener.Addr().String())
		},
		IssueCertificate: func(context.Context, string, string, int) (tls.Certificate, error) {
			return tls.Certificate{}, errors.New("unused")
		}})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	peers := []*daemonrpc.PeerUpdate{{PeerId: "first", Alias: "hp", AssignedIp: "127.100.0.4", Approved: true, Online: true, ExportedPorts: []int32{6767}}}
	if err := c.replaceGuardedRoutes(t.Context(), peers); err != nil {
		t.Fatalf("a busy browser gateway port must not fail reconciliation: %v", err)
	}
	if len(c.webRoutes) != 0 || len(c.routes) != 1 || len(names.names) != 1 {
		t.Fatalf("web routes=%d native routes=%d names=%d, want 0/1/1", len(c.webRoutes), len(c.routes), len(names.names))
	}
}

func TestCoordinatorCustomDomainAliasesWithdrawWithAuthorization(t *testing.T) {
	testCoordinatorSharedBrowserGateway(t, false, "apps.local.example.test")
}

func testCoordinatorSharedBrowserGateway(t *testing.T, nativeConflict bool, domain string) {
	t.Helper()
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	origins := map[string]*httptest.Server{}
	for _, id := range []string{"first", "second"} {
		origins[id] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, id) }))
		defer origins[id].Close()
	}
	names := &coordinatorNameClient{domain: domain, conflictPorts: map[int]bool{6767: nativeConflict}}
	c, err := NewCoordinator(CoordinatorConfig{LocalAccess: config.LocalAccessConfig{ServiceAliases: []config.LocalServiceAlias{{MachineAlias: "hp", Name: "jellyfin", Port: 6767}, {MachineAlias: "hp", Name: "ungranted", Port: 6768}}}, NameClient: names, DialMachine: func(ctx context.Context, machineID string, port int) (net.Conn, error) {
		origin := origins[machineID]
		if origin == nil || port != 6767 {
			return nil, errors.New("wrong exact machine/port")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Listener.Addr().String())
	}, IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) {
		cert, key, err := ca.IssueCertificate([]string{name})
		if err != nil {
			return tls.Certificate{}, err
		}
		return tls.X509KeyPair(cert, key)
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	peers := []*daemonrpc.PeerUpdate{{PeerId: "first", Alias: "hp", AssignedIp: "127.100.0.4", Approved: true, Online: true, ExportedPorts: []int32{6767}}, {PeerId: "second", Alias: "victus", AssignedIp: "127.100.0.5", Approved: true, Online: true, ExportedPorts: []int32{6767}}}
	if err := c.replaceGuardedRoutes(t.Context(), peers); err != nil {
		t.Fatal(err)
	}
	if len(c.webRoutes) != 1 || len(c.BrowserURLs()) != 2 {
		t.Fatal("gateway duplicated or machine URLs collapsed")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	for i, peer := range peers {
		host := "6767." + peer.Alias + "." + domain
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}}
		client := &http.Client{Transport: tr}
		req, _ := http.NewRequest(http.MethodGet, "https://"+names.addresses[443]+"/", nil)
		req.Host = host
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		tr.CloseIdleConnections()
		if err != nil || response.StatusCode != 200 || string(body) != peers[i].PeerId {
			t.Fatalf("wrong machine: %q %v", body, err)
		}
	}
	// The machine URL is a local index, while a port URL forwards the exact app.
	baseHost := "hp." + domain
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: baseHost, MinVersion: tls.VersionTLS12}}
	indexClient := &http.Client{Transport: tr}
	req, _ := http.NewRequest(http.MethodGet, "https://"+names.addresses[443]+"/", nil)
	req.Host = baseHost
	response, err := indexClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	indexBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	tr.CloseIdleConnections()
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(indexBody), "https://6767.hp."+domain+"/") {
		t.Fatalf("base HTTPS index: %s %v", indexBody, err)
	}
	aliasHost := "jellyfin.hp." + domain
	aliasTransport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: aliasHost}}
	defer aliasTransport.CloseIdleConnections()
	aliasRequest, _ := http.NewRequest(http.MethodGet, "https://"+names.addresses[443]+"/", nil)
	aliasRequest.Host = aliasHost
	aliasResponse, aliasErr := (&http.Client{Transport: aliasTransport}).Do(aliasRequest)
	if aliasErr != nil {
		t.Fatal(aliasErr)
	}
	aliasBody, _ := io.ReadAll(aliasResponse.Body)
	aliasResponse.Body.Close()
	if aliasResponse.StatusCode != 200 || string(aliasBody) != "first" {
		t.Fatalf("alias forwarded wrong resource: %s", aliasBody)
	}
	webBefore := c.webRoutes[splitdns.BrowserGatewayHostname]
	if _, err := webBefore.proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: "ungranted.hp." + domain}); err == nil {
		t.Fatal("alias granted access to unexported port")
	}
	if !strings.Contains(string(indexBody), "https://"+aliasHost+"/") {
		t.Fatal("index omitted alias")
	}
	if c.BrowserURLs()["first"][6767] != "https://6767.hp."+domain {
		t.Fatal("alias replaced numeric URL")
	}
	if err := c.replaceGuardedRoutes(t.Context(), peers[1:]); err != nil {
		t.Fatal(err)
	}
	if _, exists := c.BrowserURLs()["first"]; exists {
		t.Fatal("withdrawn machine remains published")
	}
	web := c.webRoutes[splitdns.BrowserGatewayHostname]
	if _, err := web.proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: aliasHost}); err == nil {
		t.Fatal("withdrawn TLS route remains admitted")
	}
	if err := c.replaceGuardedRoutes(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if len(c.webRoutes) != 0 || len(c.BrowserURLs()) != 0 {
		t.Fatal("empty snapshot retained gateway")
	}
}

func TestCoordinatorLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 2. Setup Coordinator with a protected local IPC socket.
	tmpDir := shorttemp.Dir(t)
	if err := os.Chmod(tmpDir, 0700); err != nil {
		t.Fatal(err)
	}
	sockPath := socketAddress(tmpDir)
	names := &coordinatorNameClient{}

	coord, err := NewCoordinator(CoordinatorConfig{
		SocketAddress: sockPath,
		AccountID:     "user_test",
		NameClient:    names,
		DialMachine:   func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") },
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	if err := coord.Start(ctx); err != nil {
		t.Fatalf("coord.Start: %v", err)
	}
	defer func() { _ = coord.Stop() }()

	// 3. Connect via daemonrpc client and verify peer table got populated
	var client *daemonrpc.Client
	for i := 0; i < 20; i++ {
		client, err = daemonrpc.NewClient(ctx, sockPath)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("connect daemonrpc client: %v", err)
	}
	defer client.Close()

	coord.applySnapshot([]*daemonrpc.PeerUpdate{{PeerId: "peer-workstation", Alias: "workstation", AssignedIp: "127.100.0.11", Online: true, Approved: true, ExportedPorts: []int32{3000, 8080}}}, 1)
	// Wait for the peer update
	var resolvedIP string
	for i := 0; i < 30; i++ {
		addr, err := client.ResolveMachine(ctx, "workstation", "user_test")
		if err == nil && addr != nil && addr.AssignedIp == "127.100.0.11" {
			resolvedIP = addr.AssignedIp
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resolvedIP != "127.100.0.11" {
		t.Fatalf("expected resolved IP 127.100.0.11, got %q", resolvedIP)
	}
	var published bool
	for i := 0; i < 20 && !published; i++ {
		names.mu.Lock()
		_, published = names.names["workstation.local.example.test"]
		names.mu.Unlock()
		if !published {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !published {
		t.Fatal("guarded reachable peer name was not published")
	}

	// 4. Verify Coordinator Stop
	if err := coord.Stop(); err != nil {
		t.Fatalf("coord.Stop: %v", err)
	}
}

func TestCoordinatorGuardedNamesFollowFullSnapshots(t *testing.T) {
	dir := shorttemp.Dir(t)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	names := &coordinatorNameClient{}
	coordinator, err := NewCoordinator(CoordinatorConfig{SocketAddress: socketAddress(dir), NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") }})
	if err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	peer := &daemonrpc.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	coordinator.applySnapshot([]*daemonrpc.PeerUpdate{peer}, 1)
	names.mu.Lock()
	_, present := names.names["studio.local.example.test"]
	names.mu.Unlock()
	if !present {
		t.Fatal("reachable guarded name missing")
	}
	names.mu.Lock()
	names.failAcquire = errors.New("guard unavailable")
	names.mu.Unlock()
	replacement := &daemonrpc.PeerUpdate{PeerId: "other", Alias: "other", AssignedIp: "127.100.0.23", Approved: true, Online: true, ExportedPorts: []int32{9090}}
	coordinator.applySnapshot([]*daemonrpc.PeerUpdate{replacement}, 2)
	remainingAfterFailure := -1
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		names.mu.Lock()
		remainingAfterFailure = len(names.names)
		names.failAcquire = nil
		names.mu.Unlock()
		if remainingAfterFailure == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if remainingAfterFailure != 0 {
		t.Fatal("guard failure retained stale names")
	}
	coordinator.applySnapshot(nil, 2)
	remaining := -1
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		names.mu.Lock()
		remaining = len(names.names)
		names.mu.Unlock()
		if remaining == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if remaining != 0 {
		t.Fatalf("withdrawn names = %v", names.names)
	}
	if err = coordinator.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorPortConflictPreservesOtherRoutesRecoversAndRevokes(t *testing.T) {
	names := &coordinatorNameClient{conflictPorts: map[int]bool{5000: true}}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	t.Cleanup(func() { _ = c.Stop() })
	peers := []*daemonrpc.PeerUpdate{
		{PeerId: "local", Alias: "apple", AssignedIp: "127.100.0.5", Approved: true, Online: true, ExportedPorts: []int32{5000, 7000}},
		{PeerId: "remote", Alias: "hp", AssignedIp: "127.100.0.4", Approved: true, Online: true, ExportedPorts: []int32{6768}},
	}
	assertRoutes := func(want int, localPorts []int32) {
		t.Helper()
		c.mu.Lock()
		count := len(c.routes)
		c.mu.Unlock()
		if count != want {
			t.Fatalf("acquired routes=%d want=%d", count, want)
		}
		status, err := c.rpcBackend.GetStatus(t.Context())
		if err != nil || len(status.Peers) != 2 {
			t.Fatalf("independent peer status withdrawn: %v %v", status, err)
		}
		for _, peer := range status.Peers {
			if peer.PeerId == "local" && !slices.Equal(peer.ForwardedPorts, localPorts) {
				t.Fatalf("reported unacquired ports: %v want=%v", peer.ForwardedPorts, localPorts)
			}
		}
	}
	c.applySnapshot(peers, 1)
	assertRoutes(2, []int32{7000})
	names.mu.Lock()
	if len(names.names) != 2 || names.addresses[5000] != "" {
		names.mu.Unlock()
		t.Fatal("conflict withdrew unrelated names or acquired a blocked listener")
	}
	delete(names.conflictPorts, 5000)
	names.mu.Unlock()
	c.applySnapshot(peers, 2)
	assertRoutes(3, []int32{5000, 7000})
	if !slices.Equal(peers[0].ExportedPorts, []int32{5000, 7000}) {
		t.Fatal("status projection mutated authoritative discovery")
	}
	names.mu.Lock()
	withdrawnAddress := names.addresses[6768]
	names.mu.Unlock()
	peers[1].Approved = false
	c.applySnapshot(peers, 3)
	c.mu.Lock()
	remaining := len(c.routes)
	c.mu.Unlock()
	names.mu.Lock()
	_, staleName := names.names["hp.local.example.test"]
	names.mu.Unlock()
	if remaining != 2 || staleName {
		t.Fatal("revocation retained a route or removed an unrelated machine")
	}
	if connection, err := net.DialTimeout("tcp", withdrawnAddress, time.Second); err == nil {
		connection.Close()
		t.Fatal("revoked listener remained reachable")
	}
}

func TestCoordinatorInventoryUsesAuthorizedDiscoveredPortsAndWithdrawsExpiry(t *testing.T) {
	dir := shorttemp.Dir(t)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{SocketAddress: socketAddress(dir), NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unused") }})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	machines := []api.UserMachine{{ID: "machine", Alias: "studio", AssignedIP: "127.100.0.22", State: "online", Online: true, NetworkApproved: true, InstallationGeneration: 2}}
	services := []api.MachineServicesMachine{{MachineID: "machine", InstallationGeneration: 2, ExpiresAt: time.Now().Add(100 * time.Millisecond), Services: []api.MachineServicePort{{Port: 45678, AddressFamily: "ipv4"}}}}
	if err := c.ApplyMachines(t.Context(), machines, services); err != nil {
		t.Fatal(err)
	}
	names.mu.Lock()
	_, discovered := names.addresses[45678]
	_, hardcoded := names.addresses[3000]
	names.mu.Unlock()
	if !discovered || hardcoded {
		t.Fatal("inventory did not use actual authorized service discovery")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		names.mu.Lock()
		remaining := len(names.names)
		names.mu.Unlock()
		if remaining == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	names.mu.Lock()
	remaining := len(names.names)
	names.mu.Unlock()
	if remaining != 0 {
		t.Fatal("expired discovery retained usable names")
	}
	services[0].ExpiresAt = time.Now().Add(time.Hour)
	machines[0].NetworkApproved = false
	if err := c.ApplyMachines(t.Context(), machines, services); err != nil {
		t.Fatal(err)
	}
	names.mu.Lock()
	remaining = len(names.names)
	names.mu.Unlock()
	if remaining != 0 {
		t.Fatal("unapproved machine regained local routes")
	}
}

func TestCoordinatorRawForwardPreservesServerFirstBytes(t *testing.T) {
	client, local := net.Pipe()
	defer client.Close()
	upstream, server := net.Pipe()
	defer server.Close()
	c := &Coordinator{cfg: CoordinatorConfig{DialMachine: func(context.Context, string, int) (net.Conn, error) { return upstream, nil }}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.forward(ctx, local, "machine", 5432) }()
	go func() { _, _ = server.Write([]byte("server greeting")) }()
	_ = client.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	got := make([]byte, len("server greeting"))
	if _, err := io.ReadFull(client, got); err != nil || string(got) != "server greeting" {
		t.Fatalf("native forwarding delayed or altered server-first protocol: %v", err)
	}
	_ = client.Close()
	_ = server.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("native forward did not retire")
	}
}

func TestCoordinatorNativePortPreservesApplicationTLSAndALPN(t *testing.T) {
	dir := shorttemp.Dir(t)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	appCA, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := appCA.IssueCertificate([]string{"studio.local.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	application, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"application-protocol"}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, err := application.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, err = conn.Write([]byte("application TLS"))
		serverDone <- err
	}()
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{SocketAddress: socketAddress(dir), NameClient: names,
		DialMachine: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", application.Addr().String())
		},
		IssueCertificate: func(_ context.Context, hostname, _ string, _ int) (tls.Certificate, error) {
			if hostname != "24443.studio."+testBrowserDomain {
				return tls.Certificate{}, errors.New("native port must not issue a local certificate")
			}
			leaf, key, err := appCA.IssueCertificate([]string{hostname})
			if err != nil {
				return tls.Certificate{}, err
			}
			return tls.X509KeyPair(leaf, key)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	c.applySnapshot([]*daemonrpc.PeerUpdate{{PeerId: "machine", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{24443}}}, 1)
	names.mu.Lock()
	address := names.addresses[24443]
	names.mu.Unlock()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(appCA.CertPEM()) {
		t.Fatal("invalid application CA")
	}
	client, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, &tls.Config{RootCAs: roots, ServerName: "studio.local.example.test", NextProtos: []string{"application-protocol"}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal("application TLS was intercepted:", err)
	}
	defer client.Close()
	if client.ConnectionState().NegotiatedProtocol != "application-protocol" {
		t.Fatal("application ALPN was changed")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	got := make([]byte, len("application TLS"))
	if _, err := io.ReadFull(client, got); err != nil || string(got) != "application TLS" {
		t.Fatalf("application TLS payload failed: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

type coordinatorNameClient struct {
	domain        string // the guard's local access domain; testBrowserDomain when empty
	mu            sync.Mutex
	names         map[string]netip.Addr
	failAcquire   error
	conflictPorts map[int]bool
	blockAcquire  bool
	addresses     map[int]string
	gateway       map[int]string // gateway-address listeners by port
	gatewayPorts  []int          // every gateway acquisition attempted, in order
	failReplace   bool
	closed        bool
	// aliasesReplaced is set by ReplaceAliases: like the real guard, names are known only after it.
	aliasesReplaced atomic.Bool
	claims          []string
}

func (c *coordinatorNameClient) Acquire(ctx context.Context, hostname string, _ netip.Addr, requestedPort int) (net.Listener, error) {
	c.mu.Lock()
	if hostname == splitdns.BrowserGatewayHostname {
		c.gatewayPorts = append(c.gatewayPorts, requestedPort)
	}
	fail := c.failAcquire
	conflict := c.conflictPorts[requestedPort]
	block := c.blockAcquire
	c.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	if conflict {
		return nil, machineguard.ErrPortInUse
	}
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		c.mu.Lock()
		if c.addresses == nil {
			c.addresses = make(map[int]string)
		}
		if hostname == splitdns.BrowserGatewayHostname {
			if c.gateway == nil {
				c.gateway = make(map[int]string)
			}
			c.gateway[requestedPort] = listener.Addr().String()
		}
		if hostname != splitdns.BrowserGatewayHostname || requestedPort == 80 || requestedPort == 443 {
			c.addresses[requestedPort] = listener.Addr().String()
		}
		c.mu.Unlock()
	}
	return listener, err
}
func (c *coordinatorNameClient) AcquireMachine(ctx context.Context, _, _, hostname string, ip netip.Addr, port int) (net.Listener, netip.Addr, error) {
	listener, err := c.Acquire(ctx, hostname, ip, port)
	return listener, ip, err
}
func (c *coordinatorNameClient) ReplaceClaims(_ context.Context, account string, machines []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims = append(c.claims, account+":"+strings.Join(machines, ","))
	return nil
}
func (c *coordinatorNameClient) ReplaceNames(_ context.Context, names map[string]netip.Addr) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if c.failReplace {
		return net.ErrClosed
	}
	c.names = maps.Clone(names)
	return nil
}
func (c *coordinatorNameClient) Close() error {
	c.mu.Lock()
	c.closed = true
	c.names = nil
	c.mu.Unlock()
	return nil
}

func TestCoordinatorBoundsGuardReconciliationAndWithdrawsNames(t *testing.T) {
	names := &coordinatorNameClient{names: map[string]netip.Addr{"stale.local.example.test": netip.MustParseAddr("127.100.0.44")}, blockAcquire: true}
	coordinator, err := NewCoordinator(CoordinatorConfig{
		NameClient: names, ReconcileTimeout: 20 * time.Millisecond,
		DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") },
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ctx = t.Context()
	started := time.Now()
	reconcileCtx, cancel := context.WithTimeout(context.Background(), coordinator.cfg.ReconcileTimeout)
	defer cancel()
	err = coordinator.replaceGuardedRoutes(reconcileCtx, []*daemonrpc.PeerUpdate{{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("bounded reconcile error=%v duration=%v", err, time.Since(started))
	}
	remaining := -1
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		names.mu.Lock()
		remaining = len(names.names)
		names.mu.Unlock()
		if remaining == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if remaining != 0 {
		t.Fatal("timed out reconciliation retained stale names")
	}
}

type generationNameClient struct {
	mu              sync.Mutex
	generation      int
	failReplace     bool
	closed          bool
	addresses       map[int]string
	acquires        int
	releases        int
	stallRelease    <-chan struct{}
	releaseFailure  error
	activeListeners []net.Listener
}

func (c *generationNameClient) AcquireMachine(ctx context.Context, _, _, hostname string, ip netip.Addr, port int) (net.Listener, netip.Addr, error) {
	listener, err := c.Acquire(ctx, hostname, ip, port)
	return listener, ip, err
}
func (c *generationNameClient) ReplaceClaims(context.Context, string, []string) error { return nil }
func (c *generationNameClient) Acquire(_ context.Context, hostname string, _ netip.Addr, port int) (net.Listener, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, net.ErrClosed
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	if c.addresses == nil {
		c.addresses = make(map[int]string)
	}
	if hostname != splitdns.BrowserGatewayHostname || port == 80 || port == 443 {
		c.addresses[port] = listener.Addr().String()
	}
	c.acquires++
	wrapped := &generationListener{Listener: listener, owner: c}
	c.activeListeners = append(c.activeListeners, wrapped)
	return wrapped, nil
}

func (c *generationNameClient) ReplaceNames(context.Context, map[string]netip.Addr) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.failReplace {
		return net.ErrClosed
	}
	return nil
}

func (c *generationNameClient) Close() error {
	c.mu.Lock()
	c.closed = true
	listeners := append([]net.Listener(nil), c.activeListeners...)
	c.mu.Unlock()
	for _, listener := range listeners {
		if wrapped, ok := listener.(*generationListener); ok {
			_ = wrapped.Listener.Close()
		}
	}
	return nil
}

type generationListener struct {
	net.Listener
	owner *generationNameClient
	once  sync.Once
}

func (l *generationListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() {
		if l.owner.stallRelease != nil {
			<-l.owner.stallRelease
		}
		l.owner.mu.Lock()
		l.owner.releases++
		l.owner.mu.Unlock()
	})
	return errors.Join(err, l.owner.releaseFailure)
}

func TestCoordinatorServesMachineHostOnPublishedPorts(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "app on "+r.Host) }))
	defer origin.Close()
	names := &coordinatorNameClient{}
	domain := testBrowserDomain
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(ctx context.Context, _ string, port int) (net.Conn, error) {
		if port != 6767 {
			return nil, errors.New("wrong exact machine/port")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Listener.Addr().String())
	}, IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) {
		cert, key, err := ca.IssueCertificate([]string{name})
		if err != nil {
			return tls.Certificate{}, err
		}
		return tls.X509KeyPair(cert, key)
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	peers := []*daemonrpc.PeerUpdate{{PeerId: "first", Alias: "hp", AssignedIp: "127.100.0.4", Approved: true, Online: true, ExportedPorts: []int32{22, 6767}}}
	if err := c.replaceGuardedRoutes(t.Context(), peers); err != nil {
		t.Fatal(err)
	}
	names.mu.Lock()
	gatewayPorts, address := slices.Clone(names.gatewayPorts), names.gateway[6767]
	names.mu.Unlock()
	if !slices.Contains(gatewayPorts, 6767) || slices.Contains(gatewayPorts, 22) {
		t.Fatalf("gateway acquired ports %v: want 6767 served and privileged 22 skipped", gatewayPorts)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	host := "hp." + domain
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequest(http.MethodGet, "https://"+address+"/", nil)
	request.Host = host + ":6767"
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "app on ") {
		t.Fatalf("https://%s:6767 answered %d %q", host, response.StatusCode, body)
	}
}

func TestCoordinatorServiceLeaseRenewalKeepsTheForwardedPortsUp(t *testing.T) {
	dir := shorttemp.Dir(t)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{SocketAddress: socketAddress(dir), NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unused") }})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	machines := []api.UserMachine{{ID: "machine", Alias: "studio", AssignedIP: "127.100.0.22", State: "online", Online: true, NetworkApproved: true, InstallationGeneration: 2}}
	services := []api.MachineServicesMachine{{MachineID: "machine", InstallationGeneration: 2, ExpiresAt: time.Now().Add(100 * time.Millisecond), Services: []api.MachineServicePort{{Port: 45678, AddressFamily: "ipv4"}}}}
	if err := c.ApplyMachines(t.Context(), machines, services); err != nil {
		t.Fatal(err)
	}
	// Renew the lease before it ends, as the daemon's own short cadence does, and wait past the
	// first lease: the forwarded port must not have been withdrawn.
	renewed := []api.MachineServicesMachine{services[0]}
	renewed[0].ExpiresAt = time.Now().Add(time.Hour)
	if err := c.ApplyServices(t.Context(), renewed); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	names.mu.Lock()
	_, kept := names.addresses[45678]
	names.mu.Unlock()
	if !kept {
		t.Fatal("a renewed lease still withdrew the forwarded port when the first one ended")
	}
}

// A service that starts on a machine adds a route to the running browser gateway; the gateway is not
// stopped and rebuilt, so a request right after the port appears is served instead of meeting a TLS
// internal error or a refused connection (Lane H: private-https to a fresh port).
func TestCoordinatorBrowserGatewayAddsARouteWithoutRestarting(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "fresh") }))
	defer origin.Close()
	names := &coordinatorNameClient{domain: testBrowserDomain}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Listener.Addr().String())
	}, IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) {
		if !names.aliasesReplaced.Load() {
			return tls.Certificate{}, errors.New("certificate for a name the guard does not know yet")
		}
		cert, key, err := ca.IssueCertificate([]string{name})
		if err != nil {
			return tls.Certificate{}, err
		}
		return tls.X509KeyPair(cert, key)
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	first := []*daemonrpc.PeerUpdate{{PeerId: "first", Alias: "hp", AssignedIp: "127.100.0.4", Approved: true, Online: true, ExportedPorts: []int32{6767}}}
	if err := c.replaceGuardedRoutes(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	gateway := c.webRoutes[splitdns.BrowserGatewayHostname].proxy
	if gateway == nil || names.addresses[443] == "" {
		t.Fatal("the browser gateway did not start its :443 listener (round 22 regression)")
	}
	second := append(first, &daemonrpc.PeerUpdate{PeerId: "second", Alias: "victus", AssignedIp: "127.100.0.5", Approved: true, Online: true, ExportedPorts: []int32{6767}})
	if err := c.replaceGuardedRoutes(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if c.webRoutes[splitdns.BrowserGatewayHostname].proxy != gateway {
		t.Fatal("a new route restarted the browser gateway")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	host := "6767.victus." + testBrowserDomain
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}}}
	request, _ := http.NewRequest(http.MethodGet, "https://"+names.addresses[443]+"/", nil)
	request.Host = host
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("new route on the running gateway: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "fresh" {
		t.Fatalf("status=%d body=%q", response.StatusCode, body)
	}
}

// A browser request for a port of a known machine whose route has not arrived yet (the service just
// started) waits for the route instead of failing the TLS handshake (Lane H: tlsv1 alert internal
// error on a fresh port); the gateway asks for a services refresh meanwhile.
func TestCoordinatorBrowserGatewayWaitsForAJustStartedServiceRoute(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "fresh") }))
	defer origin.Close()
	names := &coordinatorNameClient{domain: testBrowserDomain}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Listener.Addr().String())
	}, IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) {
		cert, key, err := ca.IssueCertificate([]string{name})
		if err != nil {
			return tls.Certificate{}, err
		}
		return tls.X509KeyPair(cert, key)
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	peer := func(ports ...int32) []*daemonrpc.PeerUpdate {
		return []*daemonrpc.PeerUpdate{{PeerId: "first", Alias: "hp", AssignedIp: "127.100.0.4", Approved: true, Online: true, ExportedPorts: ports}}
	}
	if err := c.replaceGuardedRoutes(t.Context(), peer(6767)); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-c.ServicesNudge() // the gateway asked for a refresh; the new service's route arrives
		time.Sleep(300 * time.Millisecond)
		if err := c.replaceGuardedRoutes(t.Context(), peer(6767, 7000)); err != nil {
			t.Error(err)
		}
	}()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	host := "7000.hp." + testBrowserDomain
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}}}
	request, _ := http.NewRequest(http.MethodGet, "https://"+names.addresses[443]+"/", nil)
	request.Host = host
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request for a just-started service: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "fresh" {
		t.Fatalf("status=%d body=%q", response.StatusCode, body)
	}
}

// refusingNameClient refuses one port as another OS user's, as the guard does on a shared host.
type refusingNameClient struct {
	*coordinatorNameClient
	refused int
}

func (c refusingNameClient) AcquireMachine(ctx context.Context, account, machine, hostname string, ip netip.Addr, port int) (net.Listener, netip.Addr, error) {
	if port == c.refused {
		return nil, netip.Addr{}, fmt.Errorf("acquire: %w", machineguard.ErrListenerRefused)
	}
	return c.coordinatorNameClient.AcquireMachine(ctx, account, machine, hostname, ip, port)
}

// A second OS user on a host failed its whole reconcile when the guard refused one listener as the first
// user's (multi-account hosts, 2026-10-26): only that listener is skipped.
func TestCoordinatorRefusedListenerSkipsOnlyThatItem(t *testing.T) {
	names := refusingNameClient{coordinatorNameClient: &coordinatorNameClient{}, refused: 3000}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unused") }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	peers := []*daemonrpc.PeerUpdate{
		{PeerId: "first", Alias: "hp", AssignedIp: "127.100.0.4", Approved: true, Online: true, ExportedPorts: []int32{3000, 6767}},
		{PeerId: "second", Alias: "nas", AssignedIp: "127.100.0.5", Approved: true, Online: true, ExportedPorts: []int32{3000}},
	}
	if err := c.replaceGuardedRoutes(t.Context(), peers); err != nil {
		t.Fatalf("one refused listener failed the reconcile: %v", err)
	}
	if len(c.routes) != 1 || len(names.names) != 1 {
		t.Fatalf("routes=%d names=%d, want only hp:6767", len(c.routes), len(names.names))
	}
}

// relocatingNameClient assigns another address, as the guard does when another OS user holds the preferred one.
type relocatingNameClient struct{ *coordinatorNameClient }

func (c relocatingNameClient) AcquireMachine(ctx context.Context, account, machine, hostname string, _ netip.Addr, port int) (net.Listener, netip.Addr, error) {
	return c.coordinatorNameClient.AcquireMachine(ctx, account, machine, hostname, netip.MustParseAddr("127.100.0.77"), port)
}

// PLAN 3b multi-account hosts: the daemon publishes the address the guard assigned, not the server's
// preferred one, and sends the account's complete machine set from the inventory (offline machines included)
// so the guard releases only machines that left it.
func TestCoordinatorUsesTheGuardAssignedAddressAndSendsTheCompleteSet(t *testing.T) {
	names := relocatingNameClient{&coordinatorNameClient{}}
	c := startClaimsCoordinator(t, names)
	machines := []api.UserMachine{
		{ID: "m_hp", Alias: "hp", AssignedIP: "127.100.0.3", State: "online", Online: true, NetworkApproved: true, InstallationGeneration: 1},
		{ID: "m_off", Alias: "nas", AssignedIP: "127.100.0.4", State: "offline", NetworkApproved: true},
	}
	services := []api.MachineServicesMachine{{MachineID: "m_hp", InstallationGeneration: 1, ExpiresAt: time.Now().Add(time.Hour), Services: []api.MachineServicePort{{Port: 45680, AddressFamily: "ipv4"}}}}
	if err := c.ApplyMachines(t.Context(), machines, services); err != nil {
		t.Fatal(err)
	}
	names.mu.Lock()
	got, claims := names.names["hp."+c.localDomain()], slices.Clone(names.claims)
	names.mu.Unlock()
	if got.String() != "127.100.0.77" {
		t.Fatalf("published %v, want the guard-assigned 127.100.0.77", got)
	}
	if len(claims) != 1 || claims[0] != "acct_alice:m_hp,m_off" {
		t.Fatalf("claims=%v, want the complete set including the offline machine", claims)
	}
}

// F23-03: a server outage releases nothing. While the inventory cannot be read the daemon keeps sending the
// last complete set (expired services withdraw routes, never claims), and an empty answer sends no set at all,
// so the guard's claims and 7-day holds stay as they were.
func TestCoordinatorOutageThenEmptyListReleasesNoClaim(t *testing.T) {
	names := &coordinatorNameClient{}
	c := startClaimsCoordinator(t, names)
	machines := []api.UserMachine{
		{ID: "m_a", Alias: "a", AssignedIP: "127.100.0.3", State: "online", Online: true, NetworkApproved: true, InstallationGeneration: 1},
		{ID: "m_b", Alias: "b", AssignedIP: "127.100.0.4", State: "online", Online: true, NetworkApproved: true, InstallationGeneration: 1},
	}
	services := []api.MachineServicesMachine{{MachineID: "m_a", InstallationGeneration: 1, ExpiresAt: time.Now().Add(50 * time.Millisecond), Services: []api.MachineServicePort{{Port: 45679, AddressFamily: "ipv4"}}}}
	if err := c.ApplyMachines(t.Context(), machines, services); err != nil {
		t.Fatal(err)
	}
	// Outage: no inventory read succeeds, the services lease expires and the daemon withdraws its routes, and
	// every machine goes offline in the last known view.
	time.Sleep(200 * time.Millisecond)
	for i := range machines {
		machines[i].Online, machines[i].State = false, "offline"
	}
	if err := c.ApplyServices(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	// The outage ends with an empty answer.
	if err := c.ApplyMachines(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	names.mu.Lock()
	claims := slices.Clone(names.claims)
	names.mu.Unlock()
	for _, sent := range claims {
		if sent != "acct_alice:m_a,m_b" {
			t.Fatalf("claims sent %v: an outage or an empty answer must never shrink the set", claims)
		}
	}
	if len(claims) == 0 {
		t.Fatal("the complete set was never sent")
	}
}

func startClaimsCoordinator(t *testing.T, names guardedNameClient) *Coordinator {
	t.Helper()
	dir := shorttemp.Dir(t)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c, err := NewCoordinator(CoordinatorConfig{AccountID: "acct_alice", SocketAddress: socketAddress(dir), NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unused") }})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	return c
}
