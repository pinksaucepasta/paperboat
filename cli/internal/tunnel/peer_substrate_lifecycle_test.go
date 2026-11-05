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
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
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
			got, consumed, err := peer.acquireNativeRuntime(ctx, accountID, endpointID, identity, nil)
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

// A warm `pb ping` refreshed the signed peer network on the server before every probe although a
// probe creates no grant; a runtime that already admits the target is reused without it.
func TestCachedNativeRuntimeSkipsRefreshWhenItAlreadyAdmitsTheTarget(t *testing.T) {
	issuer, accountID, endpointID := "https://api.example.test", "account_connected", "cli_connected"
	identity, _, _ := connectedEndpointAuthority(t, connectedStore(t), issuer, accountID, endpointID, "machine_connected")
	defer identity.Clear()
	networkAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: connectedStore(t), Issuer: issuer, Self: tailnet.NetworkBinding{AccountID: accountID, EndpointID: endpointID, Role: "cli", EndpointGeneration: identity.LocalCertificate.Claims.Generation, QUICCertificateFingerprint: identity.LocalCertificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.LocalCertificate.Claims.QUICPublicKey)}, Keys: connectedNetworkKeys{public: identity.RootPublic}})
	if err != nil {
		t.Fatal(err)
	}
	defer networkAuthority.Close()
	var refreshes atomic.Int32
	current := &cliNativeRuntime{accountID: accountID, endpointID: endpointID, identityFingerprint: identity.LocalCertificate.Fingerprint(), authority: networkAuthority, done: make(chan struct{})}
	peer := &PeerTerminalTunnel{nativeRuntime: current, config: PeerTerminalConfig{Issuer: issuer, Auth: &rotatingNativeAuth{token: "fixture-token"}, HTTPClient: &http.Client{Transport: substrateTransport(func(*http.Request) (*http.Response, error) {
		refreshes.Add(1)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"peer_network_unavailable","message":"unavailable"}}`))}, nil
	})}}}
	got, consumed, err := peer.acquireNativeRuntime(t.Context(), accountID, endpointID, identity, func(network *tailnet.Authority) bool { return network == networkAuthority })
	if err != nil || got != current || consumed || refreshes.Load() != 0 {
		t.Fatalf("admitting runtime: same=%t consumed=%t refreshes=%d err=%v", got == current, consumed, refreshes.Load(), err)
	}
	// F5-07/08: a stream the host admits from its own rules reuses the engine with no request, even offline.
	if got, _, err := peer.acquireNativeRuntime(t.Context(), accountID, endpointID, identity, hostRulesAdmission(&resolver.TerminalTarget{Auth: resolver.AuthTarget{Offline: true}}, "machine_connected", "managed_ssh")); err != nil || got != current || refreshes.Load() != 0 {
		t.Fatalf("host-rules stream: same=%t refreshes=%d err=%v", got == current, refreshes.Load(), err)
	}
	if admitted := hostRulesAdmission(&resolver.TerminalTarget{Auth: resolver.AuthTarget{Token: "credential"}}, "machine_connected", "managed_ssh"); admitted == nil || admitted(networkAuthority) {
		t.Fatal("a credentialed stream reused missing signed peer authority")
	}
	if _, _, err := peer.acquireNativeRuntime(t.Context(), accountID, endpointID, identity, func(*tailnet.Authority) bool { return false }); err == nil || refreshes.Load() == 0 {
		t.Fatalf("a runtime that does not admit the target was reused without refresh: refreshes=%d err=%v", refreshes.Load(), err)
	}
}

// pbvm4 prefix1 07:48:03: `pb ssh` needed no refresh (refresh_skipped at 04.925) but waited on the daemon's
// runtime lock behind two exec refreshes (03.633-04.105, 04.105-04.922) and took 1.9 s against 0.7 s. A
// refresh in flight must not hold up an operation the runtime already admits.
func TestCachedNativeRuntimeRefreshDoesNotHoldAdmittedOperations(t *testing.T) {
	issuer, accountID, endpointID := "https://api.example.test", "account_connected", "cli_connected"
	identity, _, _ := connectedEndpointAuthority(t, connectedStore(t), issuer, accountID, endpointID, "machine_connected")
	defer identity.Clear()
	networkAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: connectedStore(t), Issuer: issuer, Self: tailnet.NetworkBinding{AccountID: accountID, EndpointID: endpointID, Role: "cli", EndpointGeneration: identity.LocalCertificate.Claims.Generation, QUICCertificateFingerprint: identity.LocalCertificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.LocalCertificate.Claims.QUICPublicKey)}, Keys: connectedNetworkKeys{public: identity.RootPublic}})
	if err != nil {
		t.Fatal(err)
	}
	defer networkAuthority.Close()
	inFlight, release := make(chan struct{}), make(chan struct{})
	current := &cliNativeRuntime{accountID: accountID, endpointID: endpointID, identityFingerprint: identity.LocalCertificate.Fingerprint(), authority: networkAuthority, done: make(chan struct{})}
	peer := &PeerTerminalTunnel{nativeRuntime: current, config: PeerTerminalConfig{Issuer: issuer, Auth: &rotatingNativeAuth{token: "fixture-token"}, HTTPClient: &http.Client{Transport: substrateTransport(func(request *http.Request) (*http.Response, error) {
		close(inFlight)
		select {
		case <-release:
		case <-request.Context().Done():
		}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"peer_network_unavailable","message":"unavailable"}}`))}, nil
	})}}}
	refreshed := make(chan error, 1)
	go func() {
		_, _, err := peer.acquireNativeRuntime(t.Context(), accountID, endpointID, identity, func(*tailnet.Authority) bool { return false })
		refreshed <- err
	}()
	<-inFlight
	admitted := make(chan *cliNativeRuntime, 1)
	go func() {
		got, _, _ := peer.acquireNativeRuntime(t.Context(), accountID, endpointID, identity, func(*tailnet.Authority) bool { return true })
		admitted <- got
	}()
	select {
	case got := <-admitted:
		if got != current {
			t.Fatal("admitted operation did not reuse the runtime")
		}
	case <-time.After(time.Second):
		t.Fatal("an admitted operation waited for another operation's refresh")
	}
	close(release)
	if err := <-refreshed; err == nil {
		t.Fatal("refresh against an unavailable server admitted the operation")
	}
}

// pbvm4 quiet1-3: every `pb exec` refreshed the signed peer network (median ~300 ms) because its descriptor had just
// minted a grant. A host-rules exec (D1) carries no grant, so 10 consecutive execs to one target reuse the cached
// runtime with no network refresh at all.
func TestConsecutiveHostRulesExecsMakeNoNetworkRefresh(t *testing.T) {
	issuer, accountID, endpointID := "https://api.example.test", "account_connected", "cli_connected"
	identity, _, _ := connectedEndpointAuthority(t, connectedStore(t), issuer, accountID, endpointID, "machine_connected")
	defer identity.Clear()
	networkAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: connectedStore(t), Issuer: issuer, Self: tailnet.NetworkBinding{AccountID: accountID, EndpointID: endpointID, Role: "cli", EndpointGeneration: identity.LocalCertificate.Claims.Generation, QUICCertificateFingerprint: identity.LocalCertificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.LocalCertificate.Claims.QUICPublicKey)}, Keys: connectedNetworkKeys{public: identity.RootPublic}})
	if err != nil {
		t.Fatal(err)
	}
	defer networkAuthority.Close()
	var requests atomic.Int32
	current := &cliNativeRuntime{accountID: accountID, endpointID: endpointID, identityFingerprint: identity.LocalCertificate.Fingerprint(), authority: networkAuthority, done: make(chan struct{})}
	peer := &PeerTerminalTunnel{nativeRuntime: current, config: PeerTerminalConfig{Issuer: issuer, Auth: &rotatingNativeAuth{token: "fixture-token"}, HTTPClient: &http.Client{Transport: substrateTransport(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"peer_network_unavailable","message":"unavailable"}}`))}, nil
	})}}}
	target := &resolver.TerminalTarget{Auth: resolver.AuthTarget{Offline: true}}
	for i := range 10 {
		got, _, err := peer.acquireNativeRuntime(t.Context(), accountID, endpointID, identity, hostRulesAdmission(target, "machine_connected", nativeCapability("exec")))
		if err != nil || got != current {
			t.Fatalf("exec %d: runtime reused=%v err=%v", i, got == current, err)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("10 host-rules execs made %d network refreshes, want 0", n)
	}
}
