package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	hostserver "github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
)

// The control-plane responses are fixture data; PostgreSQL tests own grant
// minting authority. The production CLI/session adapter, authenticated native
// QUIC transport, receiver stream admission, and TCP bridge are real here.
func TestDeviceAccessUsesProductionNativeSession(t *testing.T) {
	connectedProductionRuntime(t, "device_service")
}

func TestDeviceAccessNamespaceProxyUsesProductionNativeSession(t *testing.T) {
	connectedProductionRuntime(t, "device_proxy")
}

type connectedDeviceAccess struct {
	origin      net.Listener
	revoked     chan struct{}
	deny        atomic.Bool
	grants      atomic.Int32
	origins     atomic.Int32
	discoveries atomic.Int32
	protocols   chan bool
	originReady chan struct{}
	mu          sync.Mutex
	issued      map[string]string
	flows       sync.WaitGroup
}

func newConnectedDeviceAccess(t *testing.T) *connectedDeviceAccess {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &connectedDeviceAccess{origin: l, revoked: make(chan struct{}), protocols: make(chan bool, 8), originReady: make(chan struct{}, 8), issued: make(map[string]string)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			d.origins.Add(1)
			d.originReady <- struct{}{}
			d.flows.Add(1)
			go func() {
				defer d.flows.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				payload, e := io.ReadAll(c)
				if e == nil {
					_, _ = c.Write(append([]byte("reply:"), payload...))
				}
			}()
		}
	}()
	t.Cleanup(func() { l.Close(); <-done; d.flows.Wait() })
	return d
}
func (d *connectedDeviceAccess) control(r *http.Request, identity clientauthority.Authority, network connectedNetworkAPI) (*http.Response, error) {
	if r.Header.Get("Authorization") != "Bearer device-test-token" {
		return nil, errors.New("missing authenticated CLI credential")
	}
	var data any
	switch r.URL.Path {
	case "/v1/machines":
		data = map[string]any{"items": []any{}, "pagination": map[string]any{}}
	case "/v1/device-services":
		d.discoveries.Add(1)
		data = map[string]any{"items": []api.DeviceServicesDevice{{MachineID: "machine_connected", InstallationGeneration: 1, ExpiresAt: time.Now().Add(time.Minute)}}}
	case "/v1/e2ee/root":
		k := identity.TrustedKeys[0]
		data = api.E2EERoot{Version: 1, TrustedKeys: []api.E2EEKey{{KeyID: k.KeyID, PublicKey: base64.RawURLEncoding.EncodeToString(k.PublicKey), Fingerprint: hex.EncodeToString(k.Fingerprint[:]), Generation: 1}}}
	case "/v1/endpoints/machine_connected/certificates/1":
		c := identity.MachineCertificate
		fp := c.Fingerprint()
		data = api.EndpointCertificateDocument{Version: 1, AccountID: c.Claims.AccountID, KeyID: identity.MachineCertificateKeyID, EndpointID: c.Claims.EndpointID, Role: "machine", Generation: 1, Serial: c.Claims.Serial, IssuedAt: c.Claims.IssuedAt.Format(time.RFC3339), ExpiresAt: c.Claims.ExpiresAt.Format(time.RFC3339), Certificate: base64.RawURLEncoding.EncodeToString(identity.MachineCertificateRaw), CertificateFingerprint: fp}
	case "/v1/peer-network/config":
		var err error
		data, err = network.PeerNetworkConfiguration(r.Context(), "")
		if err != nil {
			return nil, err
		}
	case "/v1/native-private-access/grants":
		var in api.NativePrivateGrantRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if in.ResourceKind != "device_service" || in.ResourceID != "machine_connected" || in.RouteID != fmt.Sprintf("tcp:%d", d.origin.Addr().(*net.TCPAddr).Port) || in.Protocol != "tcp" {
			return nil, errors.New("wrong exact-port grant request")
		}
		if d.deny.Load() {
			return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"forbidden","message":"revoked"}}`)), Request: r}, nil
		}
		count := d.grants.Add(1)
		g := api.NativePrivateGrant{Credential: fmt.Sprintf("device-credential-%d", count), ExpiresAt: time.Now().Add(time.Minute)}
		g.Target.AccountID = "account_connected"
		g.Target.UserID = "user_connected"
		g.Target.EnvironmentID = "env_connected"
		g.Target.CLIClientSessionID = "cli_connected"
		g.Target.MachineID = "machine_connected"
		g.Target.AccessSessionID = "access_connected"
		g.Target.ResourceKind = "device_service"
		g.Target.ResourceID = "machine_connected"
		g.Target.ResourceGeneration = 1
		g.Target.RouteID = in.RouteID
		g.Target.RouteGeneration = 1
		g.Target.TargetGeneration = 1
		g.Target.Protocol = "tcp"
		g.Target.TargetScheme = "tcp"
		g.Target.TargetAddress = d.origin.Addr().String()
		g.Target.InstallationGeneration = 1
		g.Target.BootID = "boot-connected-012345"
		g.Target.PolicyGeneration = 1
		g.Target.AnnouncementGeneration = 1
		d.mu.Lock()
		d.issued[g.Credential] = in.OperationID
		d.mu.Unlock()
		data = g
	default:
		return nil, fmt.Errorf("unexpected connected device request %s", r.URL.Path)
	}
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}
func (d *connectedDeviceAccess) serve(ctx context.Context, s *native.Session) error {
	d.protocols <- s.IsPrivateHTTP3()
	if s.IsPrivateHTTP3() {
		return errors.New("device TCP negotiated HTTP/3 instead of raw native streams")
	}
	for {
		c, h, err := s.AcceptAuthorized(ctx, func(_ context.Context, h streamauth.Header) (string, error) {
			d.mu.Lock()
			operation, ok := d.issued[h.Credential]
			d.mu.Unlock()
			b, e := nativeprivate.Decode([]byte(h.Target), time.Now())
			if e != nil || !ok || h.OperationID != operation || h.Consumer != "private_tcp" || b.ResourceKind != "device_service" || b.TargetAddress != d.origin.Addr().String() || b.CLIClientSessionID != "cli_connected" || b.AccessSessionID != "access_connected" {
				return "", errors.New("exact device stream binding rejected")
			}
			return "access_connected", nil
		})
		if err != nil {
			return err
		}
		d.flows.Add(1)
		go func() {
			defer d.flows.Done()
			defer c.Close()
			_ = hostserver.ServeNativePrivateTCP(ctx, h, c, func(_ context.Context, b nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
				return b.ExpiresAt, d.revoked, nil
			}, func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			})
		}()
	}
}
func (d *connectedDeviceAccess) exercise(t *testing.T, ctx context.Context, store config.ProfileStore, httpClient *http.Client, runtime *cliNativeRuntime, identity clientauthority.Authority) {
	t.Helper()
	const issuer = "https://api.example.test"
	if err := store.Save(config.Profile{Issuer: issuer, Account: config.Account{ID: "account_connected"}, CLIClientSessionID: "cli_connected", AccessExpiresAt: time.Now().Add(time.Hour)}, config.Credential{AccessToken: "device-test-token", RefreshToken: "refresh-test-token"}); err != nil {
		t.Fatal(err)
	}
	runtime.identityFingerprint = identity.LocalCertificate.Fingerprint()
	tunnel := &PeerTerminalTunnel{config: PeerTerminalConfig{Issuer: issuer, Store: store, Auth: &rotatingNativeAuth{token: "device-test-token"}, HTTPClient: httpClient, Now: time.Now}, authorities: clientauthority.NewCache(), nativeRuntime: runtime}
	defer tunnel.authorities.Close()
	access, err := preview.NewNativePrivateTCPAccess(preview.NativePrivateTCPAccessConfig{Grants: api.New(issuer, config.Credential{AccessToken: "device-test-token"}, httpClient), DialSession: tunnel.DialPrivateSession, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	port := d.origin.Addr().(*net.TCPAddr).Port
	for i := 0; i < 2; i++ {
		c, e := access.DialDevice(ctx, "machine_connected", port)
		if e != nil {
			select {
			case h3 := <-d.protocols:
				t.Fatalf("DialDevice: %v; HTTP3=%v", e, h3)
			default:
				t.Fatalf("DialDevice: %v", e)
			}
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, e = c.Write([]byte("payload")); e != nil {
			t.Fatal(e)
		}
		if e = c.(interface{ CloseWrite() error }).CloseWrite(); e != nil {
			t.Fatal(e)
		}
		got, e := io.ReadAll(c)
		c.Close()
		if e != nil || string(got) != "reply:payload" {
			t.Fatalf("half-close reply=%q err=%v", got, e)
		}
		if tunnel.nativeRuntime != runtime {
			t.Fatal("device access replaced shared runtime")
		}
	}
	if d.grants.Load() != 2 || d.discoveries.Load() != 2 {
		t.Fatalf("fresh grants=%d service discovery=%d", d.grants.Load(), d.discoveries.Load())
	}
	c, err := access.DialDevice(ctx, "machine_connected", port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 3; i++ {
		select {
		case <-d.originReady:
		case <-ctx.Done():
			t.Fatal("origin did not accept authorized flow")
		}
	}
	close(d.revoked)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var one [1]byte
	_, err = c.Read(one[:])
	if err == nil {
		t.Fatal("revoked stream remained readable")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("revoked stream did not close")
	}
	d.deny.Store(true)
	before := d.origins.Load()
	beforeDiscovery := d.discoveries.Load()
	if conn, e := access.DialDevice(ctx, "machine_connected", port); e == nil {
		conn.Close()
		t.Fatal("revoked grant opened device")
	}
	if d.origins.Load() != before || d.discoveries.Load() != beforeDiscovery {
		t.Fatal("grant rejection probed session or origin")
	}
}

func (d *connectedDeviceAccess) exerciseProxy(t *testing.T, ctx context.Context, store config.ProfileStore, httpClient *http.Client, runtime *cliNativeRuntime, identity clientauthority.Authority) {
	t.Helper()
	const issuer = "https://api.example.test"
	if err := store.Save(config.Profile{Issuer: issuer, Account: config.Account{ID: "account_connected"}, CLIClientSessionID: "cli_connected", AccessExpiresAt: time.Now().Add(time.Hour)}, config.Credential{AccessToken: "device-test-token", RefreshToken: "refresh-test-token"}); err != nil {
		t.Fatal(err)
	}
	runtime.identityFingerprint = identity.LocalCertificate.Fingerprint()
	tunnel := &PeerTerminalTunnel{config: PeerTerminalConfig{Issuer: issuer, Store: store, Auth: &rotatingNativeAuth{token: "device-test-token"}, HTTPClient: httpClient, Now: time.Now}, authorities: clientauthority.NewCache(), nativeRuntime: runtime}
	defer tunnel.authorities.Close()
	access, err := preview.NewNativePrivateTCPAccess(preview.NativePrivateTCPAccessConfig{Grants: api.New(issuer, config.Credential{AccessToken: "device-test-token"}, httpClient), DialSession: tunnel.DialPrivateSession, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	port := d.origin.Addr().(*net.TCPAddr).Port
	proxyCtx, cancelProxy := context.WithCancel(ctx)
	proxy, err := privatepreviewproxy.Start(proxyCtx, privatepreviewproxy.Config{ListenAddress: "127.0.0.1:0", MaximumConnections: 4, Dial: func(openCtx context.Context) (io.ReadWriteCloser, error) {
		return access.DialDevice(openCtx, "machine_connected", port)
	}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := strings.TrimPrefix(proxy.URL, "http://")
	local, err := net.DialTimeout("tcp4", endpoint, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = local.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = local.Write([]byte("namespace")); err != nil {
		t.Fatal(err)
	}
	if err = local.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(local)
	_ = local.Close()
	if err != nil || string(reply) != "reply:namespace" {
		t.Fatalf("proxy half-close reply=%q err=%v", reply, err)
	}
	if d.grants.Load() != 2 || d.discoveries.Load() != 2 || d.origins.Load() != 2 {
		t.Fatalf("preflight+flow grants=%d discoveries=%d origins=%d", d.grants.Load(), d.discoveries.Load(), d.origins.Load())
	}
	for i := 0; i < 2; i++ {
		select {
		case <-d.originReady:
		case <-time.After(5 * time.Second):
			t.Fatal("authorized proxy flow did not reach receiver")
		}
	}
	active, err := net.DialTimeout("tcp4", endpoint, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.originReady:
	case <-time.After(5 * time.Second):
		t.Fatal("active proxy flow did not reach receiver")
	}
	close(d.revoked)
	_ = active.SetReadDeadline(time.Now().Add(3 * time.Second))
	var one [1]byte
	if _, err = active.Read(one[:]); err == nil {
		t.Fatal("revoked proxied flow remained readable")
	}
	_ = active.Close()
	d.deny.Store(true)
	beforeOrigins, beforeDiscoveries := d.origins.Load(), d.discoveries.Load()
	denied, err := net.DialTimeout("tcp4", endpoint, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = denied.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err = denied.Read(one[:]); err == nil {
		t.Fatal("denied new proxy flow remained open")
	}
	_ = denied.Close()
	if d.origins.Load() != beforeOrigins || d.discoveries.Load() != beforeDiscoveries {
		t.Fatal("denied grant probed native session or origin")
	}
	cancelProxy()
	if err = proxy.Wait(); err != nil {
		t.Fatal(err)
	}
	if err = proxy.Close(); err != nil {
		t.Fatal(err)
	}
}
