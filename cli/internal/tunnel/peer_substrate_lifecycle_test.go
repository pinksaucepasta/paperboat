package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

type substrateAuth struct {
	signedIn  atomic.Bool
	attempted chan struct{}
}

func (a *substrateAuth) Credential() (config.Credential, error) {
	select {
	case a.attempted <- struct{}{}:
	default:
	}
	if !a.signedIn.Load() {
		return config.Credential{}, errors.New("not signed in")
	}
	return config.Credential{AccessToken: "fixture-token"}, nil
}

type substrateTransport func(*http.Request) (*http.Response, error)

func (f substrateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPeerSubstrateStartsOfflineRecoversCredentialsAndJoinsOnClose(t *testing.T) {
	root := t.TempDir()
	auth := &substrateAuth{attempted: make(chan struct{}, 8)}
	recovered := make(chan struct{}, 1)
	blocked := make(chan struct{}, 1)
	var stall atomic.Bool
	peer, err := NewPeerTerminalTunnel(PeerTerminalConfig{
		Issuer: "https://control.example.test", TLS: &tls.Config{MinVersion: tls.VersionTLS13}, Auth: auth,
		Store: config.ProfileStore{Path: filepath.Join(root, "profiles.json"), Secrets: config.FileSecretStore{Dir: filepath.Join(root, "secrets")}},
		HTTPClient: &http.Client{Transport: substrateTransport(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer fixture-token" {
				return nil, errors.New("missing refreshed authentication")
			}
			if stall.Load() {
				select {
				case blocked <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			select {
			case recovered <- struct{}{}:
			default:
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":{"regions":[]}}`))}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := peer.Start(ctx); err != nil {
		t.Fatalf("offline local startup: %v", err)
	}
	select {
	case <-auth.attempted:
	case <-time.After(3 * time.Second):
		t.Fatal("regional scan did not attempt authentication")
	}
	peer.networkMu.Lock()
	monitor := peer.sharedMonitor
	peer.networkMu.Unlock()
	if monitor == nil {
		t.Fatal("offline startup lost local network owner")
	}
	if err := peer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	peer.regionalMu.Lock()
	regional := peer.sharedRegional
	peer.regionalMu.Unlock()
	auth.signedIn.Store(true)
	regional.NetworkChanged()
	select {
	case <-recovered:
	case <-time.After(3 * time.Second):
		t.Fatal("regional monitor did not recover fresh credentials")
	}
	stall.Store(true)
	regional.NetworkChanged()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("regional request did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- peer.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close did not cancel and join remote work")
	}
	select {
	case <-peer.substrateDone:
	default:
		t.Fatal("regional worker survived Close")
	}
	if err := peer.Start(ctx); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed substrate restarted: %v", err)
	}
	if runtime, consumed, err := peer.acquireNativeRuntime(ctx, "", "", clientauthority.Authority{}); runtime != nil || consumed || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed daemon admitted a new native owner: consumed=%t error=%v", consumed, err)
	}
}

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
			peer := &PeerTerminalTunnel{nativeRuntime: current, config: PeerTerminalConfig{Issuer: issuer, Auth: topologyAuthSource{credential: config.Credential{AccessToken: "fixture-token"}}, HTTPClient: &http.Client{Transport: substrateTransport(func(request *http.Request) (*http.Response, error) {
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
