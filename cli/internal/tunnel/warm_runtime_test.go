package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/tailnet"
)

// warmFixture serves the local identity and refuses every native runtime registration, counting each one and the
// most that were in flight at once.
type warmFixture struct {
	builds, inFlight, maxInFlight atomic.Int32
	block                         bool // hold each registration until its request is cancelled
}

func (f *warmFixture) tunnel(t *testing.T) (*PeerTerminalTunnel, clientauthority.Authority) {
	t.Helper()
	const issuer = "https://api.example.test"
	store := connectedStore(t)
	identity, _, _ := connectedEndpointAuthority(t, store, issuer, "account_connected", "cli_connected", "machine_connected")
	t.Cleanup(identity.Clear)
	if err := store.Save(config.Profile{Issuer: issuer, Account: config.Account{ID: "account_connected"}, CLIClientSessionID: "cli_connected", AccessExpiresAt: time.Now().Add(time.Hour)}, config.Credential{AccessToken: "machine-test-token", RefreshToken: "refresh-test-token"}); err != nil {
		t.Fatal(err)
	}
	k := identity.TrustedKeys[0]
	keys := []api.E2EEKey{{KeyID: k.KeyID, PublicKey: base64.RawURLEncoding.EncodeToString(k.PublicKey), Fingerprint: hex.EncodeToString(k.Fingerprint[:]), Generation: 1}}
	httpClient := &http.Client{Transport: substrateTransport(func(r *http.Request) (*http.Response, error) {
		var data any
		switch r.URL.Path {
		case "/v1/peer-keys":
			data = api.PeerTransportKeySet{Version: 1, TrustedKeys: keys}
		case "/v1/endpoints/cli_connected/certificates/1":
			c := identity.LocalCertificate
			data = api.EndpointCertificateDocument{Version: 1, AccountID: c.Claims.AccountID, KeyID: k.KeyID, EndpointID: c.Claims.EndpointID, Role: "cli", Generation: 1, Serial: c.Claims.Serial, IssuedAt: c.Claims.IssuedAt.Format(time.RFC3339), ExpiresAt: c.Claims.ExpiresAt.Format(time.RFC3339), Certificate: base64.RawURLEncoding.EncodeToString(identity.LocalCertificateRaw), CertificateFingerprint: c.Fingerprint()}
		default:
			f.builds.Add(1)
			n := f.inFlight.Add(1)
			for m := f.maxInFlight.Load(); n > m && !f.maxInFlight.CompareAndSwap(m, n); m = f.maxInFlight.Load() {
			}
			if f.block {
				<-r.Context().Done()
			} else {
				time.Sleep(20 * time.Millisecond)
			}
			f.inFlight.Add(-1)
			return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unavailable","message":"down"}}`)), Request: r}, nil
		}
		body, _ := json.Marshal(map[string]any{"data": data})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
	})}
	lifetime, cancel := context.WithCancel(context.Background())
	peer := &PeerTerminalTunnel{config: PeerTerminalConfig{Issuer: issuer, Store: store, Auth: &rotatingNativeAuth{token: "machine-test-token"}, HTTPClient: httpClient, TLS: &tls.Config{MinVersion: tls.VersionTLS13}, Now: time.Now}, authorities: clientauthority.NewCache(), lifetime: lifetime, cancelLifetime: cancel}
	t.Cleanup(func() { _ = peer.Close() })
	return peer, identity
}

var warmOnline = []api.UserMachine{{ID: "machine_connected", Online: true, InstallationGeneration: 1, State: "active"}}

// First opens built the engine themselves (~2 s, first-open-steps.md §12): WarmMachines resolved the identity and
// dropped it. It now builds the runtime. A refused build leaves none, later warms back off instead of registering
// on every inventory refresh, and an open still builds on demand.
func TestWarmMachinesBuildsTheNativeRuntimeAndAFailedBuildLeavesNone(t *testing.T) {
	f := &warmFixture{}
	peer, identity := f.tunnel(t)
	if err := peer.WarmMachines(t.Context(), warmOnline); err == nil || f.builds.Load() != 1 {
		t.Fatalf("warm: err=%v builds=%d, want one refused runtime build", err, f.builds.Load())
	}
	for i := 0; i < 3; i++ {
		if err := peer.WarmMachines(t.Context(), warmOnline); err != nil || f.builds.Load() != 1 {
			t.Fatalf("warm in backoff %d: err=%v builds=%d, want no registration", i, err, f.builds.Load())
		}
	}
	peer.nativeMu.Lock()
	left := peer.nativeRuntime
	peer.nativeMu.Unlock()
	if left != nil {
		t.Fatal("a refused warm build left a half-built runtime")
	}
	if _, _, err := peer.acquireNativeRuntime(t.Context(), "account_connected", "cli_connected", identity, nil); err == nil || f.builds.Load() != 2 {
		t.Fatalf("open after a failed warm: err=%v builds=%d, want it to build on demand", err, f.builds.Load())
	}
	peer.nativeMu.Lock()
	peer.warmRetryAt = time.Now().Add(-time.Second)
	peer.nativeMu.Unlock()
	if err := peer.WarmMachines(t.Context(), warmOnline); err == nil || f.builds.Load() != 3 || peer.warmDelay != 2*warmRetryFirst {
		t.Fatalf("warm after backoff: err=%v builds=%d delay=%v, want a retry and a doubled delay", err, f.builds.Load(), peer.warmDelay)
	}
	if err := peer.WarmMachines(t.Context(), []api.UserMachine{{ID: "machine_connected"}}); err != nil || f.builds.Load() != 3 {
		t.Fatalf("no reachable machine: err=%v builds=%d, want nothing started", err, f.builds.Load())
	}
}

// A stop or pb update right after start must not wait out a warm build's registration (up to 10 s).
func TestCloseDuringABlockedWarmBuildReturnsAtOnce(t *testing.T) {
	f := &warmFixture{block: true}
	peer, _ := f.tunnel(t)
	warmed := make(chan error, 1)
	go func() { warmed <- peer.WarmMachines(context.Background(), warmOnline) }()
	for deadline := time.Now().Add(10 * time.Second); f.inFlight.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the warm build never reached its registration")
		}
	}
	started := time.Now()
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("Close waited %v for the warm build", took)
	}
	if err := <-warmed; err == nil {
		t.Fatal("the cancelled warm build reported success")
	}
}

// An inventory refresh while the runtime is up reuses it with no server call, and a warm racing an open builds
// under nativeMu one at a time (run with -race).
func TestWarmMachinesReusesTheLiveRuntimeAndSerializesWithAnOpen(t *testing.T) {
	f := &warmFixture{}
	peer, identity := f.tunnel(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = peer.WarmMachines(t.Context(), warmOnline) }()
		go func() {
			defer wg.Done()
			_, _, _ = peer.acquireNativeRuntime(t.Context(), "account_connected", "cli_connected", identity, nil)
		}()
	}
	wg.Wait()
	if f.maxInFlight.Load() != 1 || f.builds.Load() == 0 {
		t.Fatalf("builds=%d max in flight=%d, want serialized builds", f.builds.Load(), f.maxInFlight.Load())
	}
	live := &cliNativeRuntime{accountID: "account_connected", endpointID: "cli_connected", identityFingerprint: identity.LocalCertificate.Fingerprint(), done: make(chan struct{})}
	peer.nativeMu.Lock()
	peer.nativeRuntime = live
	peer.nativeMu.Unlock()
	before := f.builds.Load()
	for i := 0; i < 3; i++ {
		if err := peer.WarmMachines(t.Context(), warmOnline); err != nil {
			t.Fatal(err)
		}
	}
	got, consumed, err := peer.acquireNativeRuntime(t.Context(), "account_connected", "cli_connected", identity, func(*tailnet.Authority) bool { return true })
	if err != nil || consumed || got != live || f.builds.Load() != before {
		t.Fatalf("reuse: same=%t consumed=%t err=%v server calls=%d", got == live, consumed, err, f.builds.Load()-before)
	}
	peer.nativeMu.Lock()
	peer.nativeRuntime = nil // a bare fixture runtime has no engine to Close
	peer.nativeMu.Unlock()
}
