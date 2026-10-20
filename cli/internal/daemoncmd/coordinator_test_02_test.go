package daemoncmd

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	previewruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/testcert"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

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
	// Four listeners per generation: the native port, the gateway on 80 and 443, and the gateway's own
	// listener on the machine port 8080 that serves https://<machine>.<domain>:8080.
	if connects != 2 || firstReleases != 4 || secondAcquires != 4 || secondReleases != 0 || !slices.Equal(slices.Compact(certificateGenerations), []int{1, 2}) {
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

// The protected machine-name service being down must not keep the daemon from starting: a daemon
// that exits at start crash-loops, so it cannot be updated or inspected. It starts degraded, and a
// reconciliation connects once the service is back.
func TestCoordinatorStartsWithoutTheGuardAndConnectsWhenItReturns(t *testing.T) {
	dir, err := os.MkdirTemp("", "pb-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "guard-start.sock")
	guardErr := errors.New("guard unavailable")
	guardUp := false
	names := &coordinatorNameClient{}
	coordinator, err := NewCoordinator(CoordinatorConfig{SocketAddress: path, ConnectNameClient: func(context.Context) (guardedNameClient, error) {
		if !guardUp {
			return nil, guardErr
		}
		return names, nil
	}, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("must not dial") }})
	if err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Start(t.Context()); err != nil {
		t.Fatalf("the daemon must start while the guard is down: %v", err)
	}
	defer coordinator.Stop()
	if conn, err := net.Dial("unix", path); err != nil {
		t.Fatalf("daemon IPC is unavailable: %v", err)
	} else {
		conn.Close()
	}
	if err = coordinator.replaceGuardedRoutes(t.Context(), nil); !errors.Is(err, guardErr) {
		t.Fatalf("reconciliation did not report the guard failure: %v", err)
	}
	guardUp = true
	if err = coordinator.replaceGuardedRoutes(t.Context(), nil); err != nil {
		t.Fatalf("reconciliation did not recover when the guard returned: %v", err)
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
	if names.names["studio.local.example.test"].String() != peer.AssignedIp {
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

// The fake guards answer Status like the real one: every owner's domain is the test domain.
func testGuardStatus(context.Context) (machineguard.RuntimeStatus, error) {
	return machineguard.RuntimeStatus{BrowserDomain: testBrowserDomain, DefaultBrowserDomain: testBrowserDomain}, nil
}
func (c *coordinatorNameClient) Status(ctx context.Context) (machineguard.RuntimeStatus, error) {
	if c.domain != "" {
		return machineguard.RuntimeStatus{BrowserDomain: c.domain, DefaultBrowserDomain: c.domain}, nil
	}
	return testGuardStatus(ctx)
}
func (c *generationNameClient) Status(ctx context.Context) (machineguard.RuntimeStatus, error) {
	return testGuardStatus(ctx)
}
func (c *coordinatorCleanupClient) Status(ctx context.Context) (machineguard.RuntimeStatus, error) {
	return testGuardStatus(ctx)
}

func testBrowserHostname(t *testing.T, machine string, port int) string {
	t.Helper()
	host, err := splitdns.BrowserHostname(machine, port, testBrowserDomain)
	if err != nil {
		t.Fatal(err)
	}
	return host
}

func TestCoordinatorReportsFailedReconciliationAndWithdraws(t *testing.T) {
	guardErr := errors.New("guard unavailable")
	names := &coordinatorNameClient{failAcquire: guardErr}
	var reported error
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unexpected dial") }, ReportReconcileError: func(err error) { reported = err }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx, c.cancel = context.WithCancel(t.Context())
	defer c.Stop()
	c.applySnapshot([]*pbSync.PeerUpdate{{PeerId: "machine", Alias: "hp", AssignedIp: "127.100.0.22", Approved: true, Online: true, ExportedPorts: []int32{35173}}}, 1)
	if !errors.Is(reported, guardErr) || strings.Contains(reported.Error(), guardErr.Error()) {
		t.Fatalf("readiness failure not reported: %v", reported)
	}
	if len(c.routes) != 0 || len(c.webRoutes) != 0 || !names.closed {
		t.Fatal("failed authority left guarded routes alive")
	}
}

func TestCoordinatorStreamingReadinessReportsFailedAndRecoveredReconciliation(t *testing.T) {
	reference := supportref.New()
	ctx := supportref.WithContext(t.Context(), reference)
	names := &coordinatorCleanupClient{}
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	var reported error
	firstConnection := true
	c, err := NewCoordinator(CoordinatorConfig{DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("unexpected dial") }, ConnectNameClient: func(context.Context) (guardedNameClient, error) {
		if firstConnection {
			firstConnection = false
			return names, nil
		}
		return &coordinatorCleanupClient{}, nil
	}, ReportReconcileError: func(err error) { reported = err }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = ctx
	defer c.Stop()
	peers := []*pbSync.PeerUpdate{{PeerId: "peer", Alias: "hp", AssignedIp: "127.100.0.22", Approved: true, Online: true}}
	c.applySnapshot(peers, 1)
	if err := c.ReconcileStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	names.replaceCause = syscall.EIO
	c.applySnapshot([]*pbSync.PeerUpdate{{PeerId: "bad", Alias: "hp", AssignedIp: "PRIVATE invalid address", Approved: true, Online: true}}, 2)
	failure := c.ReconcileStatus(t.Context())
	if !errors.Is(failure, syscall.EIO) {
		t.Fatal("invalid snapshot lost independent protected-name withdrawal cause")
	}
	_, addressErr := netip.ParseAddr("PRIVATE invalid address")
	if !errors.Is(failure, addressErr) {
		t.Fatal("invalid snapshot lost original address parser cause")
	}
	if strings.Contains(failure.Error(), "PRIVATE") || reported == nil || !errors.Is(reported, syscall.EIO) || strings.Contains(reported.Error(), "PRIVATE") {
		t.Fatal("callback lost original cause or exposed private address")
	}
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference {
		t.Fatal("withdrawal failure did not use one correlated observation")
	}
	if _, err := c.rpcBackend.ResolveMachine(t.Context(), "peer"); err == nil {
		t.Fatal("invalid snapshot retained RPC readiness")
	}
	c.applySnapshot(peers, 3)
	if err := c.ReconcileStatus(t.Context()); err != nil {
		t.Fatal("fresh valid snapshot did not recover")
	}
	if _, err := c.rpcBackend.ResolveMachine(t.Context(), "peer"); err != nil {
		t.Fatal("fresh snapshot did not restore RPC readiness")
	}
	if len(faults) != 1 {
		t.Fatal("healthy recovery emitted duplicate failure")
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
	c, err := NewCoordinator(CoordinatorConfig{LocalAccess: config.LocalAccessConfig{ServiceAliases: []config.LocalServiceAlias{{MachineAlias: "homelab", Name: "bob", Port: 8080}, {MachineAlias: "homelab", Name: "jellyfin", Port: 80}}}, NameClient: names, DialMachine: func(ctx context.Context, id string, port int) (net.Conn, error) {
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

	blocked := "bob.homelab." + testBrowserDomain
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
		host := app + ".homelab." + testBrowserDomain
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
	if _, err := web.proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: "third.homelab." + testBrowserDomain}); err == nil {
		t.Fatal("unexported override fell back to default80")
	}
	peer.ExportedPorts = []int32{80, 8080}
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	web = c.webRoutes[splitdns.BrowserGatewayHostname]
	if route, ok := browserRouteForHostDomain(web.browserRoutes, "third.homelab."+testBrowserDomain, testBrowserDomain); !ok || route.Port != 8080 {
		t.Fatal("override failed to select authorized8080")
	}
	c.cfg.LocalAccess.MachineProxies = nil
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	web = c.webRoutes[splitdns.BrowserGatewayHostname]
	if route, ok := browserRouteForHostDomain(web.browserRoutes, "third.homelab."+testBrowserDomain, testBrowserDomain); !ok || route.Port != 80 {
		t.Fatal("unset override did not restore default80")
	}
	peer.ExportedPorts = []int32{8080}
	if err := c.replaceGuardedRoutes(t.Context(), []*pbSync.PeerUpdate{peer}); err != nil {
		t.Fatal(err)
	}
	web = c.webRoutes[splitdns.BrowserGatewayHostname]
	if _, err := web.proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: "third.homelab." + testBrowserDomain}); err == nil {
		t.Fatal("unauthorized proxy port certified")
	}
	w := httptest.NewRecorder()
	web.proxy.ServeHTTP(w, httptest.NewRequest("GET", "http://third.homelab."+testBrowserDomain+"/", nil))
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

func TestCoordinatorStopRetainsCleanupFailureAndRecovery(t *testing.T) {
	names := &coordinatorCleanupClient{cause: syscall.EIO}
	c, err := NewCoordinator(CoordinatorConfig{NameClient: names, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c.ctx = t.Context()
	if err := c.Stop(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("owned cleanup cause lost: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("cleanup was repeated: %v", err)
	}
	fresh, err := NewCoordinator(CoordinatorConfig{NameClient: &coordinatorCleanupClient{}, DialMachine: func(context.Context, string, int) (net.Conn, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	fresh.ctx = t.Context()
	if err := fresh.Stop(); err != nil {
		t.Fatal(err)
	}
}

type coordinatorCleanupClient struct {
	coordinatorNameClient
	cause        error
	replaceCause error
}

func (c *coordinatorCleanupClient) ReplaceNames(ctx context.Context, names map[string]netip.Addr) error {
	if c.replaceCause != nil {
		return c.replaceCause
	}
	return c.coordinatorNameClient.ReplaceNames(ctx, names)
}
func (c *coordinatorCleanupClient) Close() error { _ = c.coordinatorNameClient.Close(); return c.cause }
