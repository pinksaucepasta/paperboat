package native_test

import (
	"context"
	"crypto/ed25519"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	hostpreview "github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	hostserver "github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
)

type nativeHTTPRoutes []hostpreview.NativePrivateHTTPRoute

func (r nativeHTTPRoutes) SnapshotNativePrivateHTTP(context.Context) ([]hostpreview.NativePrivateHTTPRoute, error) {
	return append([]hostpreview.NativePrivateHTTPRoute(nil), r...), nil
}

type nativeHTTPGrantIssuer func(context.Context, api.NativePrivateGrantRequest) (api.NativePrivateGrant, error)

func (f nativeHTTPGrantIssuer) IssueNativePrivateGrant(ctx context.Context, request api.NativePrivateGrantRequest) (api.NativePrivateGrant, error) {
	return f(ctx, request)
}

func TestNativePrivateHTTP3AuthorizedDERPWorkflow(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(nil)
	clientTLS, clientFingerprint := testTLS(t, "private-http-cli")
	serverTLS, serverFingerprint := testTLS(t, "private-http-machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::11"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::12"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().UTC().Truncate(time.Second)
	applyTestConfiguration(t, clientAuthority, signerPrivate, testConfiguration(now.Unix(), 1, clientBinding, serverBinding, "dial"))
	applyTestConfiguration(t, serverAuthority, signerPrivate, testConfiguration(now.Unix(), 1, serverBinding, clientBinding, "accept"))
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()

	originListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer originListener.Close()
	go func() {
		connection, acceptErr := originListener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		payload, _ := io.ReadAll(connection)
		_, _ = connection.Write(append([]byte("http-origin:"), payload...))
	}()

	captureStore := inspector.NewStore()
	_ = captureStore.SetPolicy("prv_http", inspector.ResourcePolicy{Enabled: true, CaptureRaw: true, CaptureRequestBody: true, CaptureResponseBody: true})
	descriptor, serverReady := startNativeHTTP3Server(t, serverOwner, serverAuthority, dm.Regions[1], originListener.Addr().String(), captureStore)
	issuer := nativeHTTPGrantIssuer(func(_ context.Context, request api.NativePrivateGrantRequest) (api.NativePrivateGrant, error) {
		var grant api.NativePrivateGrant
		grant.Target.AccountID, grant.Target.UserID, grant.Target.EnvironmentID = "account_test", "account_test", "env_test"
		grant.Target.MachineID, grant.Target.AccessSessionID = "machine_test", "grant_test"
		grant.Target.ResourceKind, grant.Target.ResourceID, grant.Target.ResourceGeneration = "preview", "prv_http", 1
		grant.Target.RouteID, grant.Target.RouteGeneration, grant.Target.TargetGeneration = "prv_http", 1, 1
		grant.Target.Protocol, grant.Target.TargetScheme, grant.Target.TargetAddress = "http", "http", originListener.Addr().String()
		grant.Credential, grant.ExpiresAt = "credential_private_http", now.Add(time.Minute)
		return grant, nil
	})
	access, err := hostpreview.NewNativePrivateHTTPAccess(hostpreview.NativePrivateHTTPAccessConfig{
		Routes: nativeHTTPRoutes{{MatchType: "exact", Hostname: "preview.example.test", ResourceKind: "preview", ResourceID: "prv_http", RouteID: "prv_http"}}, Grants: issuer,
		DialSession: func(ctx context.Context, _ string) (hostpreview.NativePrivateHTTPSession, error) {
			session, dialErr := clientOwner.Dial(ctx, descriptor, "machine_test", peerquic.ClassPreview)
			if dialErr != nil {
				t.Logf("dial: %v", dialErr)
			} else {
				select {
				case <-serverReady:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return session, dialErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := access.Open(t.Context(), "preview.example.test")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("GET /private HTTP/1.1\r\nHost: preview.example.test\r\n\r\n")
	if _, err = stream.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = stream.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil || string(response) != "http-origin:"+string(payload) {
		t.Fatalf("response=%q err=%v", response, err)
	}
	observedAt := time.Now().UTC()
	page, err := captureStore.List(t.Context(), inspector.Credential{PrincipalID: "account_test", Action: inspector.ActionInspect, ResourceID: "prv_http", ResourceGeneration: 1, RouteGeneration: 1, TargetGeneration: 1, AuthorityReadAt: observedAt, ExpiresAt: observedAt.Add(time.Minute)}, "", 10)
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("native observation unavailable: %v records=%d", err, len(page.Records))
	}
	record := page.Records[0]
	if record.Method != "CONNECT" || record.State != inspector.StateUnsupported || len(record.RequestBody) != 0 || len(record.ResponseBody) != 0 || record.ReplayIneligible == "" {
		t.Fatal("opaque native connection presented as replayable HTTP")
	}

}

func startNativeHTTP3Server(t *testing.T, owner *native.Owner, authority *tailnet.Authority, region *tailcfg.DERPRegion, origin string, captureStore *inspector.Store) (tailcat.Addr, <-chan struct{}) {
	t.Helper()
	server, err := authority.Listen(region)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	ready := make(chan struct{})
	go func() {
		_ = owner.Listen(ctx, region, func(serveCtx context.Context, session *native.Session) error {
			close(ready)
			return hostserver.ServeNativePrivateHTTP3(serveCtx, session, func(_ context.Context, header streamauth.Header) (string, error) {
				if header.Credential != "credential_private_http" {
					return "", tailnet.ErrAdmission
				}
				return "grant_test", nil
			}, func(_ context.Context, binding nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
				if binding.TargetAddress != origin {
					return time.Time{}, nil, hostserver.ErrNativePrivateBinding
				}
				return binding.ExpiresAt, make(chan struct{}), nil
			}, func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}, captureStore)
		})
	}()
	return server.Address(), ready
}
