package enrollment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

// enrolledRoot enrolls a machine through a throwaway control plane (which then answers renewals as handler says) and
// leaves its credential expired, so the next Token must renew.
func enrolledRoot(t *testing.T, now time.Time, renewal http.HandlerFunc) (string, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/helper-enrollments" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": strings.Repeat("e", 40), "expires_at": now.Add(time.Hour)}})
			return
		}
		renewal(w, r)
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	if err := makeEnrollmentStateRootPrivateForTest(root); err != nil {
		t.Fatal(err)
	}
	client, _ := NewClient(server.Client().Transport, time.Second)
	identity, err := client.Enroll(context.Background(), Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: strings.Repeat("g", 40)})
	if err != nil {
		t.Fatal(err)
	}
	identity.ExpiresAt = now.Add(-time.Hour)
	if err := writeIdentity(root, identity); err != nil {
		t.Fatal(err)
	}
	return root, server
}

func renewalSourceFor(t *testing.T, url, root string, now time.Time, transport http.RoundTripper) *RenewingTokenSource {
	t.Helper()
	source, err := NewRenewingTokenSource(RenewingTokenConfig{ControlURL: url, StateRoot: root, Transport: transport, Timeout: time.Second, Clock: func() time.Time { return now }, OperationID: func() (string, error) { return "op_renew_refusal", nil }})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// A renewal the server refuses with 401 is terminal (audit: six orphan daemons retried every ~65 s for hours): one
// request, then none for that identity, from this source, another source over the same state or Renew; the refusal
// is still the observed 401; a new enrollment (a new credential) is tried again.
func TestRenewalRefusedWith401IsAttemptedOnceUntilReEnrollment(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var requests atomic.Int32
	root, server := enrolledRoot(t, now, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, `{"error":{"code":"credential_invalid"}}`, http.StatusUnauthorized)
	})
	source := renewalSourceFor(t, server.URL, root, now, server.Client().Transport)
	_, err := source.Token(context.Background())
	if fault := errorreport.ProjectFault(context.Background(), "paperboatd", "machine_control", "control_request", "control_request_failed", err); err == nil || fault.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("first renewal err=%v status=%d", err, fault.HTTPStatus)
	}
	other := renewalSourceFor(t, server.URL, root, now, server.Client().Transport)
	for name, call := range map[string]func() (string, error){
		"same source":    func() (string, error) { return source.Token(context.Background()) },
		"another source": func() (string, error) { return other.Token(context.Background()) },
		"forced renewal": func() (string, error) { return source.Renew(context.Background()) },
		"after a rejected": func() (string, error) {
			source.CredentialRejected(strings.Repeat("e", 40))
			return source.Token(context.Background())
		},
	} {
		_, err := call()
		if fault := errorreport.ProjectFault(context.Background(), "paperboatd", "machine_control", "control_request", "control_request_failed", err); err == nil || fault.HTTPStatus != http.StatusUnauthorized {
			t.Fatalf("%s: err=%v status=%d, want the refusal", name, err, fault.HTTPStatus)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("%d renewal requests, want exactly 1", got)
	}
	if !RenewalRefused(root, now) {
		t.Fatal("doctor cannot see that the machine was refused")
	}
	refusals.Delete(root) // a restarted daemon: one new attempt (a wrong clock must be able to recover), then stopped again
	_, _ = source.Token(context.Background())
	_, _ = source.Token(context.Background())
	if got := requests.Load(); got != 2 {
		t.Fatalf("%d requests after a restart, want 2", got)
	}
	// The hold ends after renewalRefusalHold: one more attempt (a wrong clock, once right, heals without re-enrollment).
	wall := time.Now()
	backoffNow = func() time.Time { return wall.Add(renewalRefusalHold + time.Minute) }
	t.Cleanup(func() { backoffNow = time.Now })
	_, _ = source.Token(context.Background())
	if got := requests.Load(); got != 3 {
		t.Fatalf("%d requests after the hold, want 3", got)
	}
	backoffNow = time.Now // the refusal recorded at the fake time still holds: only a new credential lifts it
	reenrolled, err := LoadRuntimeIdentityForRenewal(root, now)
	if err != nil {
		t.Fatal(err)
	}
	reenrolled.Credential = strings.Repeat("n", 40)
	if err := writeIdentity(root, reenrolled); err != nil {
		t.Fatal(err)
	}
	if RenewalRefused(root, now) {
		t.Fatal("a new enrollment is still reported as refused")
	}
	_, _ = source.Token(context.Background())
	if got := requests.Load(); got != 4 {
		t.Fatalf("%d requests after re-enrollment, want 4", got)
	}
}

// A control plane that answers 5xx is retried at once after the first failure (a blip), then on a doubling, jittered
// schedule that stops growing at its cap, and no request is made inside the window.
func TestRenewalBackoffGrowsAndIsCapped(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var requests atomic.Int32
	root, server := enrolledRoot(t, now, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	source := renewalSourceFor(t, server.URL, root, now, server.Client().Transport)
	wall := time.Now()
	backoffNow = func() time.Time { return wall }
	t.Cleanup(func() { backoffNow = time.Now })
	state := backoffFor(root)
	var delays []time.Duration
	for attempt := 1; attempt <= 12; attempt++ {
		before := requests.Load()
		if _, err := source.Token(context.Background()); err == nil {
			t.Fatal("renewal succeeded against a 503")
		}
		if requests.Load() != before+1 {
			t.Fatalf("attempt %d made %d requests", attempt, requests.Load()-before)
		}
		if attempt == 1 {
			continue // one failure opens no window: the next call is allowed at once
		}
		state.mu.Lock()
		delays = append(delays, state.retryAt.Sub(wall))
		state.mu.Unlock()
		if _, err := source.Token(context.Background()); err == nil || requests.Load() != before+1 {
			t.Fatalf("attempt %d: a request was made inside the backoff window (err=%v)", attempt, err)
		}
		wall = wall.Add(delays[len(delays)-1] + time.Millisecond)
	}
	if delays[0] < renewalBackoffStart/2 || delays[0] >= renewalBackoffStart*3/2 {
		t.Fatalf("first delay %v outside the jittered start %v", delays[0], renewalBackoffStart)
	}
	if delays[4] < 4*renewalBackoffStart/2 {
		t.Fatalf("delay did not grow: %v", delays)
	}
	for _, delay := range delays[len(delays)-3:] {
		if delay < renewalBackoffMaximum/2 || delay >= renewalBackoffMaximum*3/2 {
			t.Fatalf("late delay %v outside the jittered cap %v: %v", delay, renewalBackoffMaximum, delays)
		}
	}
}

// A renewal answered clock_skew (400, the server's time in the body) is a transient failure: no refusal is recorded
// (doctor does not say the machine was removed), the daemon does not stop for an hour, and the next attempt, made with
// the corrected clock, renews.
func TestRenewalClockSkewIsTransientAndNeverARefusal(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var requests atomic.Int32
	root, server := enrolledRoot(t, now, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "clock_skew", "details": map[string]any{"server_time": now.Format(time.RFC3339)}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": strings.Repeat("r", 40), "expires_at": now.Add(time.Hour)}})
	})
	source := renewalSourceFor(t, server.URL, root, now, server.Client().Transport)
	_, err := source.Token(context.Background())
	if fault := errorreport.ProjectFault(context.Background(), "paperboatd", "identity_renewal", "control_request", "control_request_failed", err); err == nil || !errors.Is(err, ErrUnavailable) || fault.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("skewed renewal err=%v status=%d, want a transient unavailable answer carrying the 400", err, fault.HTTPStatus)
	}
	if RenewalRefused(root, now) {
		t.Fatal("clock skew was recorded as a refusal")
	}
	if token, err := source.Token(context.Background()); err != nil || token != strings.Repeat("r", 40) {
		t.Fatalf("the retry did not renew: %q %v", token, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("%d requests, want 2", got)
	}
}

// Every helper proof the CLI signs (an ordinary one and the renewal proof) carries a nonce and no two share one: the
// server uses a proof once (F21-13).
func TestHelperProofsCarryAFreshNonce(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := enrolledRoot(t, now, func(w http.ResponseWriter, r *http.Request) {})
	current, err := LoadRuntimeIdentityForRenewal(root, now)
	if err != nil {
		t.Fatal(err)
	}
	current.ExpiresAt = now.Add(time.Hour)
	if err := writeIdentity(root, current); err != nil {
		t.Fatal(err)
	}
	source := ProofSource{StateRoot: root, Clock: func() time.Time { return now }}
	nonces := map[string]bool{}
	for name, sign := range map[string]func() ([]byte, error){
		"proof": func() ([]byte, error) {
			return source.Proof(context.Background(), "operation-nonce-one", http.MethodPost, "/v1/x", []byte("{}"))
		},
		"proof again": func() ([]byte, error) {
			return source.Proof(context.Background(), "operation-nonce-one", http.MethodPost, "/v1/x", []byte("{}"))
		},
		"renewal proof": func() ([]byte, error) {
			return source.renewalProof("operation-nonce-two", http.MethodPost, "/v1/helper-identity-renewals", []byte("{}"))
		},
	} {
		proof, err := sign()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var envelope struct {
			Payload string `json:"payload"`
		}
		if json.Unmarshal(proof, &envelope) != nil {
			t.Fatalf("%s: not a proof envelope", name)
		}
		payload, _ := base64.RawURLEncoding.DecodeString(envelope.Payload)
		var claims struct {
			Nonce string `json:"nonce"`
		}
		if json.Unmarshal(payload, &claims) != nil || len(claims.Nonce) < 16 || nonces[claims.Nonce] {
			t.Fatalf("%s: nonce %q is missing, short or reused", name, claims.Nonce)
		}
		nonces[claims.Nonce] = true
	}
}
