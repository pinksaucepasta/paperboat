package tunnel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

func generationTunnel(t *testing.T, respond func(*http.Request) (int, string)) (*PeerTerminalTunnel, *[]string, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var paths, reasons []string
	store := connectedStore(t)
	if err := store.Save(config.Profile{Issuer: "https://api.example.test", Account: config.Account{ID: "account_connected"}, CLIClientSessionID: "cli_connected", AccessExpiresAt: time.Now().Add(time.Hour)}, config.Credential{AccessToken: "fixture-token", RefreshToken: "refresh-token"}); err != nil {
		t.Fatal(err)
	}
	peer := &PeerTerminalTunnel{config: PeerTerminalConfig{Issuer: "https://api.example.test", Store: store, Generation: testDaemonGeneration, Auth: &rotatingNativeAuth{token: "fixture-token"},
		OnIdentityRevoked: func(reason string) { mu.Lock(); reasons = append(reasons, reason); mu.Unlock() },
		HTTPClient: &http.Client{Transport: substrateTransport(func(r *http.Request) (*http.Response, error) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["daemon_generation"] != testDaemonGeneration {
				t.Errorf("%s without this generation: %v %v", r.URL.Path, body, err)
			}
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			status, payload := respond(r)
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
		})}}}
	return peer, &paths, &reasons
}

// A drained generation withdraws once at exit; a repeat, or a withdraw after the server already revoked it, is not an
// error, and a generation's own withdraw never reports itself as revoked.
func TestGenerationDrainAndWithdrawAreIdempotent(t *testing.T) {
	var revoked atomic.Bool
	peer, paths, reasons := generationTunnel(t, func(r *http.Request) (int, string) {
		if r.URL.Path == "/v1/peer-network/withdraw" && revoked.Load() {
			return http.StatusForbidden, `{"error":{"code":"peer_network_generation_withdrawn","message":"finished"}}`
		}
		return http.StatusOK, `{"data":{"ok":true}}`
	})
	for range 2 {
		if err := peer.BeginDrain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	store, endpoint := peer.config.Store, testDaemonEndpoint("cli_connected")
	if err := store.UpdatePeerNetworkState(peer.config.Issuer, "account_connected", endpoint, func(s *config.PeerNetworkState) error {
		s.PrivateKey, s.KeyGeneration = make([]byte, 32), 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePeerNetworkConfiguration(peer.config.Issuer, "account_connected", endpoint, "cached.config.token"); err != nil {
		t.Fatal(err)
	}
	if err := peer.Withdraw(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Nothing of the withdrawn generation is left: no key custody, no cached configuration.
	if cached, err := store.LoadPeerNetworkConfiguration(peer.config.Issuer, "account_connected", endpoint, 1<<20); err != nil || cached != "" {
		t.Fatalf("cached configuration left behind: %q %v", cached, err)
	}
	if err := store.UpdatePeerNetworkState(peer.config.Issuer, "account_connected", endpoint, func(s *config.PeerNetworkState) error {
		if s.KeyGeneration != 0 || len(s.PrivateKey) != 0 {
			return errors.New("key custody left behind")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	revoked.Store(true)
	if err := peer.Withdraw(t.Context()); err != nil {
		t.Fatalf("withdraw of a revoked generation: %v", err)
	}
	want := "/v1/peer-network/drain /v1/peer-network/drain /v1/peer-network/withdraw /v1/peer-network/withdraw"
	if got := strings.Join(*paths, " "); got != want || len(*reasons) != 0 {
		t.Fatalf("paths=%q reasons=%v", got, *reasons)
	}
}

// A newer generation that pushed this one out is reported once, with its reason; the generation then never starts a
// new runtime or registers again, so it cannot revive itself and evict the newer one (no registration loop).
func TestRefusedGenerationReportsOnceAndNeverRegistersAgain(t *testing.T) {
	peer, paths, reasons := generationTunnel(t, func(*http.Request) (int, string) {
		return http.StatusForbidden, `{"error":{"code":"peer_network_generation_evicted","message":"replaced"}}`
	})
	network := peer.networkAPI()
	for range 3 {
		if _, err := network.PeerNetworkConfiguration(t.Context(), api.PeerNetworkConfigurationRequest{OperationID: "operation_refresh"}); err == nil {
			t.Fatal("evicted refresh succeeded")
		}
	}
	if strings.Join(*reasons, ",") != "generation_evicted" {
		t.Fatalf("reasons = %v", *reasons)
	}
	requests := len(*paths)
	issuer, accountID, session := "https://api.example.test", "account_connected", "cli_connected"
	identity, _, _ := connectedEndpointAuthority(t, connectedStore(t), issuer, accountID, session, "machine_connected")
	defer identity.Clear()
	if _, consumed, err := peer.acquireNativeRuntime(t.Context(), accountID, session, identity, nil); !errors.Is(err, ErrGenerationRetiring) || consumed {
		t.Fatalf("refused generation started a runtime: consumed=%t err=%v", consumed, err)
	}
	if len(*paths) != requests {
		t.Fatalf("refused generation called the server again: %v", *paths)
	}
}

func TestRefusalReason(t *testing.T) {
	for err, want := range map[error]string{
		&api.APIError{Status: http.StatusForbidden, Code: "peer_network_generation_evicted"}:   "generation_evicted",
		&api.APIError{Status: http.StatusForbidden, Code: "peer_network_generation_withdrawn"}: "revoked",
		&api.APIError{Status: http.StatusForbidden, Code: "peer_network_denied"}:               "revoked",
		&api.APIError{Status: http.StatusUnauthorized, Code: "authentication_required"}:        "revoked",
		&api.APIError{Status: http.StatusServiceUnavailable, Code: "peer_network_unavailable"}: "",
		errors.New("connection reset"): "",
	} {
		if got := refusalReason(err); got != want {
			t.Errorf("refusalReason(%v) = %q, want %q", err, got, want)
		}
	}
}
