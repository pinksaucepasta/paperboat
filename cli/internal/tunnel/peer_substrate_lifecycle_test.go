package tunnel

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

type substrateTransport func(*http.Request) (*http.Response, error)

func (f substrateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCachedNativeRuntimeRefreshesOperationAuthority(t *testing.T) {
	issuer, accountID, endpointID := "https://api.example.test", "account_connected", "cli_connected"
	identity, _, _ := connectedEndpointAuthority(t, connectedStore(t), issuer, accountID, endpointID, "machine_connected")
	defer identity.Clear()
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "control_plane_failure", true: "cancellation"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			networkAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: connectedStore(t), Issuer: issuer, Self: tailnet.NetworkBinding{AccountID: accountID, EndpointID: endpointID, Role: "cli", EndpointGeneration: identity.LocalCertificate.Claims.Generation, QUICCertificateFingerprint: identity.LocalCertificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.LocalCertificate.Claims.QUICPublicKey)}, Keys: connectedNetworkKeys{public: identity.RootPublic}})
			if err != nil {
				t.Fatal(err)
			}
			defer networkAuthority.Close()
			current := &cliNativeRuntime{accountID: accountID, endpointID: endpointID, identityFingerprint: identity.LocalCertificate.Fingerprint(), authority: networkAuthority, done: make(chan struct{})}
			peer := &PeerTerminalTunnel{nativeRuntime: current, config: PeerTerminalConfig{Issuer: issuer, Auth: &rotatingNativeAuth{token: "fixture-token"}, HTTPClient: &http.Client{Transport: substrateTransport(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				if canceled {
					cancel()
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"peer_network_unavailable","message":"unavailable"}}`))}, nil
			})}}}
			got, consumed, err := peer.acquireNativeRuntime(ctx, accountID, endpointID, identity)
			if err == nil || got != nil || consumed || calls.Load() == 0 {
				t.Fatalf("cached authority admitted without successful refresh: runtime=%t consumed=%t calls=%d error=%v", got != nil, consumed, calls.Load(), err)
			}
			if peer.nativeRuntime != current {
				t.Fatal("failed operation discarded runtime shared by existing sessions")
			}
			if canceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("refresh did not preserve cancellation: %v", err)
			}
		})
	}
}
