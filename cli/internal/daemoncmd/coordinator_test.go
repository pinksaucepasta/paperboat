package daemoncmd

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/testcert"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	previewruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"google.golang.org/grpc"
)

func TestCoordinatorSharedBrowserGatewayRoutesExactMachinesAndWithdraws(t *testing.T) {
	testCoordinatorSharedBrowserGateway(t, false, splitdns.BrowserSuffix)
}

func TestCoordinatorBrowserGatewayPreservesRoutesWhenNativePortsConflict(t *testing.T) {
	testCoordinatorSharedBrowserGateway(t, true, splitdns.BrowserSuffix)
}

func TestCoordinatorCustomDomainAliasesWithdrawWithAuthorization(t *testing.T) {
	testCoordinatorSharedBrowserGateway(t, false, "apps.local.pprbt.dev")
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
	names := &coordinatorNameClient{conflictPorts: map[int]bool{6767: nativeConflict}}
	c, err := NewCoordinator(CoordinatorConfig{LocalAccess: config.LocalAccessConfig{Domain: domain, ServiceAliases: []config.LocalServiceAlias{{MachineAlias: "hp", Name: "jellyfin", Port: 6767}, {MachineAlias: "hp", Name: "ungranted", Port: 6768}}}, NameClient: names, DialMachine: func(ctx context.Context, machineID string, port int) (net.Conn, error) {
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
	peers := []*pbSync.PeerUpdate{{PeerId: "first", Alias: "hp", AssignedIp: "127.100.0.4", Approved: true, Online: true, ExportedPorts: []int32{6767}}, {PeerId: "second", Alias: "victus", AssignedIp: "127.100.0.5", Approved: true, Online: true, ExportedPorts: []int32{6767}}}
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

type testControlSyncServer struct {
	pbSync.UnimplementedSyncServiceServer
}

func (s *testControlSyncServer) Sync(stream pbSync.SyncService_SyncServer) error {
	_, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&pbSync.SyncResponse{
		Revision:   1,
		AssignedIp: "127.100.0.10",
		Peers: []*pbSync.PeerUpdate{
			{
				PeerId:        "peer-workstation",
				Alias:         "workstation",
				AssignedIp:    "127.100.0.11",
				Online:        true,
				Approved:      true,
				ExportedPorts: []int32{3000, 8080},
				Tags:          []string{"dev"},
			},
		},
	}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestCoordinatorLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Launch a test control plane sync server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mock sync: %v", err)
	}
	defer listener.Close()

	grpcServer := grpc.NewServer()
	pbSync.RegisterSyncServiceServer(grpcServer, &testControlSyncServer{})
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()

	// 2. Setup Coordinator with a protected local IPC socket.
	tmpDir := t.TempDir()
	if err := os.Chmod(tmpDir, 0700); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(tmpDir, "test-daemon.sock")
	names := &coordinatorNameClient{}

	coord, err := NewCoordinator(CoordinatorConfig{
		SocketAddress: sockPath,
		SyncAddress:   listener.Addr().String(),
		MachineID:     "machine-local",
		Token:         func(context.Context) (string, error) { return "test", nil },
		Insecure:      true,
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

	// Wait for peer update from mock sync server
	var resolvedIP string
	for i := 0; i < 30; i++ {
		addr, err := client.ResolveMachine(ctx, "workstation")
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
		_, published = names.names["workstation.local.pprbt.dev"]
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

func TestCoordinatorFailsBeforeIPCWhenSyncCannotAuthenticate(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "daemon.sock")
	coordinator, err := NewCoordinator(CoordinatorConfig{SocketAddress: socket, SyncAddress: "127.0.0.1", MachineID: "machine", Token: func(context.Context) (string, error) { return "token", nil }, Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Start(t.Context()); err == nil {
		t.Fatal("invalid sync endpoint reported ready")
	}
	if _, statErr := os.Lstat(socket); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("IPC was published after sync failure: %v", statErr)
	}
	if err = coordinator.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorGuardedNamesFollowFullSnapshots(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	names := &coordinatorNameClient{}
	coordinator, err := NewCoordinator(CoordinatorConfig{SocketAddress: filepath.Join(dir, "daemon.sock"), NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") }})
	if err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	coordinator.applySnapshot([]*pbSync.PeerUpdate{peer}, 1)
	names.mu.Lock()
	_, present := names.names["studio.local.pprbt.dev"]
	names.mu.Unlock()
	if !present {
		t.Fatal("reachable guarded name missing")
	}
	names.mu.Lock()
	names.failAcquire = true
	names.mu.Unlock()
	replacement := &pbSync.PeerUpdate{PeerId: "other", Alias: "other", AssignedIp: "127.100.0.23", Approved: true, Online: true, ExportedPorts: []int32{9090}}
	coordinator.applySnapshot([]*pbSync.PeerUpdate{replacement}, 2)
	remainingAfterFailure := -1
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		names.mu.Lock()
		remainingAfterFailure = len(names.names)
		names.failAcquire = false
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
	peers := []*pbSync.PeerUpdate{
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
	_, staleName := names.names["hp.local.pprbt.dev"]
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
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{SocketAddress: filepath.Join(dir, "daemon.sock"), NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unused") }})
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
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	appCA, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := appCA.IssueCertificate([]string{"studio.local.pprbt.dev"})
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
	c, err := NewCoordinator(CoordinatorConfig{SocketAddress: filepath.Join(dir, "daemon.sock"), NameClient: names,
		DialMachine: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", application.Addr().String())
		},
		IssueCertificate: func(_ context.Context, hostname, _ string, _ int) (tls.Certificate, error) {
			if hostname != "24443.studio."+splitdns.BrowserSuffix {
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
	c.applySnapshot([]*pbSync.PeerUpdate{{PeerId: "machine", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{24443}}}, 1)
	names.mu.Lock()
	address := names.addresses[24443]
	names.mu.Unlock()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(appCA.CertPEM()) {
		t.Fatal("invalid application CA")
	}
	client, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, &tls.Config{RootCAs: roots, ServerName: "studio.local.pprbt.dev", NextProtos: []string{"application-protocol"}, MinVersion: tls.VersionTLS12})
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
	mu            sync.Mutex
	names         map[string]netip.Addr
	failAcquire   bool
	conflictPorts map[int]bool
	blockAcquire  bool
	addresses     map[int]string
	failReplace   bool
	closed        bool
}

func (c *coordinatorNameClient) Acquire(ctx context.Context, _ string, _ netip.Addr, requestedPort int) (net.Listener, error) {
	c.mu.Lock()
	fail := c.failAcquire
	conflict := c.conflictPorts[requestedPort]
	block := c.blockAcquire
	c.mu.Unlock()
	if fail {
		return nil, errors.New("guard unavailable")
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
		c.addresses[requestedPort] = listener.Addr().String()
		c.mu.Unlock()
	}
	return listener, err
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
	names := &coordinatorNameClient{names: map[string]netip.Addr{"stale.local.pprbt.dev": netip.MustParseAddr("127.100.0.44")}, blockAcquire: true}
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
	err = coordinator.replaceGuardedRoutes(reconcileCtx, []*pbSync.PeerUpdate{{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}})
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
	activeListeners []net.Listener
}

func (c *generationNameClient) Acquire(_ context.Context, _ string, _ netip.Addr, port int) (net.Listener, error) {
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
	c.addresses[port] = listener.Addr().String()
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
	return err
}

func TestCoordinatorReconnectsNameClientOnNextSnapshot(t *testing.T) {
	first := &generationNameClient{generation: 1}
	second := &generationNameClient{generation: 2}
	clients := []*generationNameClient{first, second}
	connects := 0
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	var certificateGenerations []int
	opened := make(chan net.Conn, 1)
	coordinator, err := NewCoordinator(CoordinatorConfig{
		ConnectNameClient: func(context.Context) (guardedNameClient, error) {
			if connects >= len(clients) {
				return nil, errors.New("unexpected reconnect")
			}
			client := clients[connects]
			connects++
			return client, nil
		},
		DialMachine: func(context.Context, string, int) (net.Conn, error) {
			client, remote := net.Pipe()
			opened <- remote
			return client, nil
		},
		IssueCertificate: func(_ context.Context, hostname, _ string, _ int) (tls.Certificate, error) {
			generation := connects
			certificateGenerations = append(certificateGenerations, generation)
			certificate, key, issueErr := ca.IssueCertificate([]string{hostname})
			if issueErr != nil {
				return tls.Certificate{}, issueErr
			}
			return tls.X509KeyPair(certificate, key)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ctx = t.Context()
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err := coordinator.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	address := first.addresses[8080]
	first.mu.Unlock()
	local, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	remote := <-opened
	defer remote.Close()
	first.mu.Lock()
	first.failReplace = true
	first.mu.Unlock()
	if err := coordinator.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("generation failure=%v", err)
	}
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := remote.Read(make([]byte, 1)); err == nil {
		t.Fatal("generation failure retained active flow")
	}
	if err := coordinator.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	firstReleases := first.releases
	first.mu.Unlock()
	second.mu.Lock()
	secondAcquires, secondReleases := second.acquires, second.releases
	second.mu.Unlock()
	if connects != 2 || firstReleases != 3 || secondAcquires != 3 || secondReleases != 0 || !slices.Equal(slices.Compact(certificateGenerations), []int{1, 2}) {
		t.Fatalf("connects=%d first releases=%d second acquires=%d releases=%d certificate generations=%v", connects, firstReleases, secondAcquires, secondReleases, certificateGenerations)
	}
}

func TestCoordinatorGenerationCleanupHonorsReconcileDeadline(t *testing.T) {
	release := make(chan struct{})
	client := &generationNameClient{generation: 1, stallRelease: release}
	coordinator, err := NewCoordinator(CoordinatorConfig{NameClient: client, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unused") }})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ctx = t.Context()
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err := coordinator.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.failReplace = true
	client.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = coordinator.replaceGuardedRoutes(ctx, []*pbSync.PeerUpdate{peer})
	if !errors.Is(err, net.ErrClosed) || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("cleanup error=%v duration=%v", err, time.Since(started))
	}
	close(release)
}

func TestCoordinatorProtectedHTTPFrontDoorUsesAuthenticatedMachineDial(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	names := &coordinatorNameClient{}
	dialed := make(chan int, 1)
	coordinator, err := NewCoordinator(CoordinatorConfig{
		NameClient: names, IssueCertificate: func(_ context.Context, hostname, _ string, _ int) (tls.Certificate, error) {
			certPEM, keyPEM, issueErr := ca.IssueCertificate([]string{hostname})
			if issueErr != nil {
				return tls.Certificate{}, issueErr
			}
			return tls.X509KeyPair(certPEM, keyPEM)
		},
		DialMachine: func(_ context.Context, machineID string, port int) (net.Conn, error) {
			if machineID != "peer" {
				return nil, errors.New("wrong peer")
			}
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				_, _ = http.ReadRequest(bufio.NewReader(server))
				_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			}()
			dialed <- port
			return client, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ctx = t.Context()
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{3000}}
	if err := coordinator.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	names.mu.Lock()
	address := names.addresses[80]
	names.mu.Unlock()
	req, _ := http.NewRequest(http.MethodGet, "http://"+address+"/", nil)
	req.Host = testBrowserHostname(t, "studio", 3000)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || <-dialed != 3000 {
		t.Fatalf("status=%d", response.StatusCode)
	}
	coordinator.withdrawGuardedLocked()
}

func TestCoordinatorRawRouteDrainsDelayedResponseAfterClientHalfClose(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		conn, acceptErr := backend.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		body, _ := io.ReadAll(conn)
		if string(body) == "request" {
			time.Sleep(50 * time.Millisecond)
			_, _ = io.WriteString(conn, "delayed-response")
		}
	}()

	coordinator, names := testRawRouteCoordinator(t, func(ctx context.Context, _ string, _ int) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", backend.Addr().String())
	})
	address := installTestRawRoute(t, coordinator, names)
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(client, "request"); err != nil {
		t.Fatal(err)
	}
	if err = client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	_ = client.Close()
	if err != nil || string(response) != "delayed-response" {
		t.Fatalf("response=%q err=%v", response, err)
	}
	coordinator.withdrawGuardedLocked()
}

func TestCoordinatorWithdrawalCancelsActiveAndPendingRawRoutes(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		opened := make(chan net.Conn, 1)
		coordinator, names := testRawRouteCoordinator(t, func(context.Context, string, int) (net.Conn, error) {
			client, remote := net.Pipe()
			opened <- remote
			return client, nil
		})
		address := installTestRawRoute(t, coordinator, names)
		local, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer local.Close()
		remote := <-opened
		defer remote.Close()
		if err := coordinator.replaceGuardedRoutes(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		_ = remote.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := remote.Read(make([]byte, 1)); err == nil {
			t.Fatal("withdrawal left active remote stream open")
		}
	})

	t.Run("pending dial", func(t *testing.T) {
		started := make(chan struct{})
		canceled := make(chan struct{})
		coordinator, names := testRawRouteCoordinator(t, func(ctx context.Context, _ string, _ int) (net.Conn, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		})
		address := installTestRawRoute(t, coordinator, names)
		local, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer local.Close()
		<-started
		if err := coordinator.replaceGuardedRoutes(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("withdrawal did not cancel pending machine dial")
		}
	})
}

func testRawRouteCoordinator(t *testing.T, dial func(context.Context, string, int) (net.Conn, error)) (*Coordinator, *coordinatorNameClient) {
	t.Helper()
	names := &coordinatorNameClient{}
	coordinator, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: dial})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ctx = t.Context()
	return coordinator, names
}

func installTestRawRoute(t *testing.T, coordinator *Coordinator, names *coordinatorNameClient) string {
	t.Helper()
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}
	if err := coordinator.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	names.mu.Lock()
	address := names.addresses[8080]
	names.mu.Unlock()
	return address
}

type coordinatorNativeIssuer struct {
	mu       sync.Mutex
	requests []api.NativePrivateGrantRequest
	now      time.Time
}

func (i *coordinatorNativeIssuer) IssueNativePrivateGrant(_ context.Context, request api.NativePrivateGrantRequest) (api.NativePrivateGrant, error) {
	i.mu.Lock()
	i.requests = append(i.requests, request)
	i.mu.Unlock()
	var grant api.NativePrivateGrant
	grant.Target.AccountID, grant.Target.UserID, grant.Target.EnvironmentID = "owner", "owner", "env"
	grant.Target.MachineID, grant.Target.AccessSessionID, grant.Target.CLIClientSessionID = "peer", "access", "cli"
	grant.Target.ResourceKind, grant.Target.ResourceID, grant.Target.ResourceGeneration = "machine_service", "peer", 1
	grant.Target.RouteID, grant.Target.RouteGeneration, grant.Target.TargetGeneration = request.RouteID, 2, 3
	grant.Target.Protocol, grant.Target.TargetScheme, grant.Target.TargetAddress = "tcp", "tcp", "127.0.0.1:3000"
	grant.Target.InstallationGeneration, grant.Target.BootID = 1, "boot"
	grant.Target.PolicyGeneration, grant.Target.AnnouncementGeneration = 2, 3
	grant.Credential, grant.ExpiresAt = "credential", i.now.Add(time.Minute)
	return grant, nil
}

type coordinatorNativeSession struct {
	headers chan streamauth.Header
}

func (s *coordinatorNativeSession) OpenAuthorized(_ context.Context, header streamauth.Header, access, capability string) (net.Conn, error) {
	if access != "access" || capability != "private_access" {
		return nil, errors.New("unexpected native authority")
	}
	s.headers <- header
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		_, _ = server.Write([]byte{0})
		request, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		_ = request.Body.Close()
		_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\nnative")
	}()
	return client, nil
}

func (*coordinatorNativeSession) Close() error { return nil }

func TestCoordinatorHTTPAndTLSUseRealNativePrivateAuthority(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	issuer := &coordinatorNativeIssuer{now: now}
	headers := make(chan streamauth.Header, 2)
	access, err := previewruntime.NewNativePrivateTCPAccess(previewruntime.NativePrivateTCPAccessConfig{
		Grants: issuer,
		DialSession: func(context.Context, string) (previewruntime.NativePrivateSession, error) {
			return &coordinatorNativeSession{headers: headers}, nil
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	names := &coordinatorNameClient{}
	coordinator, err := NewCoordinator(CoordinatorConfig{
		NameClient: names, DialMachine: access.DialMachine,
		IssueCertificate: func(_ context.Context, hostname, _ string, _ int) (tls.Certificate, error) {
			certificate, key, issueErr := ca.IssueCertificate([]string{hostname})
			if issueErr != nil {
				return tls.Certificate{}, issueErr
			}
			return tls.X509KeyPair(certificate, key)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ctx = t.Context()
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{3000}}
	if err := coordinator.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	urls := coordinator.BrowserURLs()
	if urls["peer"][3000] != "https://"+testBrowserHostname(t, "studio", 3000) {
		t.Fatalf("browser URL projection=%v", urls)
	}
	urls["peer"][3000] = "mutated"
	if coordinator.BrowserURLs()["peer"][3000] == "mutated" {
		t.Fatal("browser URL snapshot aliases live state")
	}
	names.mu.Lock()
	httpAddress, httpsAddress := names.addresses[80], names.addresses[443]
	names.mu.Unlock()

	requestAndRead := func(client *http.Client, scheme, address string) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, scheme+"://"+address+"/", nil)
		request.Host = testBrowserHostname(t, "studio", 3000)
		response, requestErr := client.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "native" {
			t.Fatalf("status=%d body=%q err=%v", response.StatusCode, body, readErr)
		}
	}
	requestAndRead(http.DefaultClient, "http", httpAddress)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("append test CA")
	}
	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: testBrowserHostname(t, "studio", 3000), MinVersion: tls.VersionTLS12}}}
	requestAndRead(tlsClient, "https", httpsAddress)

	for range 2 {
		header := <-headers
		// This is the current authority boundary: each proxied request reaches
		// the native session with an exact server-issued private_tcp header and
		// canonical binding, rather than relying on the DNS name or route alone.
		if header.Consumer != "private_tcp" || header.Credential != "credential" || header.OperationID == "" || header.Target == "" {
			t.Fatalf("native header=%+v", header)
		}
	}
	issuer.mu.Lock()
	requests := append([]api.NativePrivateGrantRequest(nil), issuer.requests...)
	issuer.mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("grant requests=%d want=2", len(requests))
	}
	for _, request := range requests {
		if request.ResourceKind != "machine_service" || request.ResourceID != "peer" || request.RouteID != "tcp:3000" || request.Protocol != "tcp" || request.OperationID == "" {
			t.Fatalf("grant request=%+v", request)
		}
	}
	peer.ExportedPorts = []int32{3001}
	if err := coordinator.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	urls = coordinator.BrowserURLs()
	if len(urls["peer"]) != 1 || urls["peer"][3001] != "https://"+testBrowserHostname(t, "studio", 3001) {
		t.Fatalf("changed service URL projection=%v", urls)
	}
	names.mu.Lock()
	_, oldPublished := names.names[testBrowserHostname(t, "studio", 3000)]
	names.mu.Unlock()
	if oldPublished {
		t.Fatal("removed service alias remained published")
	}
	if err := coordinator.replaceGuardedRoutes(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if len(coordinator.BrowserURLs()) != 0 {
		t.Fatal("withdrawn browser URLs remain discoverable")
	}
}

func TestCoordinatorGuardUnavailableFailsBeforeIPC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard-start.sock")
	guardErr := errors.New("guard unavailable")
	coordinator, err := NewCoordinator(CoordinatorConfig{SocketAddress: path, ConnectNameClient: func(context.Context) (guardedNameClient, error) { return nil, guardErr }, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("must not dial") }})
	if err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Start(t.Context()); !errors.Is(err, guardErr) {
		coordinator.Stop()
		t.Fatalf("startup did not report the helper failure: %v", err)
	}
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		t.Fatal("IPC became available after failed startup")
	}
}

func TestCoordinatorWebOnlyPeerPublishesName(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	names := &coordinatorNameClient{}
	coordinator, err := NewCoordinator(CoordinatorConfig{NameClient: names, IssueCertificate: func(_ context.Context, host, _ string, _ int) (tls.Certificate, error) {
		cert, key, err := ca.IssueCertificate([]string{host})
		if err != nil {
			return tls.Certificate{}, err
		}
		return tls.X509KeyPair(cert, key)
	}, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") }})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ctx = t.Context()
	defer coordinator.Stop()
	peer := &pbSync.PeerUpdate{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{80, 443}}
	if err = coordinator.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	names.mu.Lock()
	defer names.mu.Unlock()
	if names.names["studio.local.pprbt.dev"].String() != peer.AssignedIp {
		t.Fatal("HTTP/HTTPS-only peer name was not published")
	}
	if names.addresses[80] == "" || names.addresses[443] == "" {
		t.Fatal("web listeners missing")
	}
}

func TestCoordinatorFailedProjectionWithdrawsIPCAddress(t *testing.T) {
	names := &coordinatorNameClient{}
	coordinator, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("not dialed") }})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.ctx = t.Context()
	defer coordinator.Stop()
	peers := []*pbSync.PeerUpdate{{PeerId: "peer", Alias: "studio", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{8080}}}
	coordinator.applySnapshot(peers, 1)
	if _, err = coordinator.rpcBackend.ResolveMachine(t.Context(), "peer"); err != nil {
		t.Fatal("ready route absent from IPC", err)
	}
	names.mu.Lock()
	names.failReplace = true
	names.mu.Unlock()
	coordinator.applySnapshot(peers, 2)
	if _, err = coordinator.rpcBackend.ResolveMachine(t.Context(), "peer"); err == nil {
		t.Fatal("failed local projection left a resolvable IPC address")
	}
}

func TestMapPeerSnapshotProjectsCanonicalAddressWithoutMutation(t *testing.T) {
	peer := &pbSync.PeerUpdate{PeerId: "peer", AssignedIp: "127.100.23.45", Approved: true}
	mapped, err := mapPeerSnapshot([]*pbSync.PeerUpdate{peer}, "127.212.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if mapped[0].GetAssignedIp() != "127.212.23.45" || peer.GetAssignedIp() != "127.100.23.45" {
		t.Fatalf("mapped=%q canonical=%q", mapped[0].GetAssignedIp(), peer.GetAssignedIp())
	}
	if _, err = mapPeerSnapshot([]*pbSync.PeerUpdate{{PeerId: "bad", AssignedIp: "127.211.23.45"}}, "127.212.0.0/16"); err == nil {
		t.Fatal("noncanonical server assignment accepted")
	}
}

func (c *coordinatorNameClient) ReplaceAliases(context.Context, map[string]string) error { return nil }
func (c *generationNameClient) ReplaceAliases(context.Context, map[string]string) error  { return nil }

func testBrowserHostname(t *testing.T, machine string, port int) string {
	t.Helper()
	host, err := splitdns.BrowserHostname(machine, port, splitdns.BrowserSuffix)
	if err != nil {
		t.Fatal(err)
	}
	return host
}

func TestCoordinatorReportsFailedReconciliationAndWithdraws(t *testing.T) {
	names := &coordinatorNameClient{failAcquire: true}
	var reported error
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unexpected dial") }, ReportReconcileError: func(err error) { reported = err }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx, c.cancel = context.WithCancel(t.Context())
	defer c.Stop()
	c.applySnapshot([]*pbSync.PeerUpdate{{PeerId: "machine", Alias: "hp", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{35173}}}, 1)
	if reported == nil || !strings.Contains(reported.Error(), "guard unavailable") {
		t.Fatalf("readiness failure not reported: %v", reported)
	}
	if len(c.routes) != 0 || len(c.webRoutes) != 0 || !names.closed {
		t.Fatal("failed authority left guarded routes alive")
	}
}

func TestCoordinatorStreamingReadinessReportsFailedAndRecoveredReconciliation(t *testing.T) {
	c, err := NewCoordinator(CoordinatorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c.applySnapshot([]*pbSync.PeerUpdate{{PeerId: "bad", Alias: "hp", AssignedIp: "invalid", Approved: true, Online: true}}, 1)
	if err := c.ReconcileStatus(t.Context()); err == nil {
		t.Fatal("invalid streaming snapshot reported ready")
	}
	c.applySnapshot(nil, 2)
	if err := c.ReconcileStatus(t.Context()); err != nil {
		t.Fatalf("empty valid snapshot not ready: %v", err)
	}
}

func TestCoordinatorMachineProxyFollowsAuthorizedExportAndRevocation(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.Host) }))
	defer origin.Close()
	names := &coordinatorNameClient{}
	c, err := NewCoordinator(CoordinatorConfig{LocalAccess: config.LocalAccessConfig{Domain: splitdns.BrowserSuffix, ServiceAliases: []config.LocalServiceAlias{{MachineAlias: "homelab", Name: "bob", Port: 8080}, {MachineAlias: "homelab", Name: "jellyfin", Port: 80}}}, NameClient: names, DialMachine: func(ctx context.Context, id string, port int) (net.Conn, error) {
		if id != "home" || port != 80 {
			return nil, errors.New("wrong authorized destination")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Listener.Addr().String())
	}, IssueCertificate: func(_ context.Context, name, _ string, _ int) (tls.Certificate, error) { return ca.TLS(name) }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	defer c.Stop()
	peer := &pbSync.PeerUpdate{PeerId: "home", Alias: "homelab", AssignedIp: "127.100.0.4", Approved: true, Online: true, ExportedPorts: []int32{80}}
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	web := c.webRoutes[splitdns.BrowserGatewayHostname]

	blocked := "bob.homelab." + splitdns.BrowserSuffix
	if _, err := web.proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: blocked}); err == nil {
		t.Fatal("unavailable explicit alias certified via fallback")
	}
	denied := httptest.NewRecorder()
	web.proxy.ServeHTTP(denied, httptest.NewRequest("GET", "http://"+blocked+"/", nil))
	if denied.Code != 421 {
		t.Fatal("unavailable explicit alias forwarded via fallback")
	}
	count := len(web.browserRoutes)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	for _, app := range []string{"first", "second", "third", "jellyfin"} {
		host := app + ".homelab." + splitdns.BrowserSuffix
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host}}
		req, _ := http.NewRequest("GET", "https://"+names.addresses[443]+"/", nil)
		req.Host = host
		res, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			tr.CloseIdleConnections()
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		tr.CloseIdleConnections()
		if res.StatusCode != 200 || string(body) != host {
			t.Fatalf("app forwarding: %d %s", res.StatusCode, body)
		}
	}
	if len(web.browserRoutes) != count {
		t.Fatal("application added route state")
	}

	// An explicit override selects only that port. Removing it restores port 80.
	c.cfg.LocalAccess.MachineProxies = []config.LocalMachineProxy{{MachineAlias: "homelab", Port: 8080}}
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	web = c.webRoutes[splitdns.BrowserGatewayHostname]
	if _, err := web.proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: "third.homelab." + splitdns.BrowserSuffix}); err == nil {
		t.Fatal("unexported override fell back to default80")
	}
	peer.ExportedPorts = []int32{80, 8080}
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	web = c.webRoutes[splitdns.BrowserGatewayHostname]
	if route, ok := browserRouteForHost(web.browserRoutes, "third.homelab."+splitdns.BrowserSuffix); !ok || route.Port != 8080 {
		t.Fatal("override failed to select authorized8080")
	}
	c.cfg.LocalAccess.MachineProxies = nil
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	web = c.webRoutes[splitdns.BrowserGatewayHostname]
	if route, ok := browserRouteForHost(web.browserRoutes, "third.homelab."+splitdns.BrowserSuffix); !ok || route.Port != 80 {
		t.Fatal("unset override did not restore default80")
	}
	peer.ExportedPorts = []int32{8080}
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	web = c.webRoutes[splitdns.BrowserGatewayHostname]
	if _, err := web.proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: "third.homelab." + splitdns.BrowserSuffix}); err == nil {
		t.Fatal("unauthorized proxy port certified")
	}
	w := httptest.NewRecorder()
	web.proxy.ServeHTTP(w, httptest.NewRequest("GET", "http://third.homelab."+splitdns.BrowserSuffix+"/", nil))
	if w.Code != 421 {
		t.Fatal("unauthorized proxy forwarded")
	}
	peer.Approved = false
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	if len(c.webRoutes) != 0 {
		t.Fatal("revocation retained gateway")
	}
}
