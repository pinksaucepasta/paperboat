package enrollment

import (
	"context"
	"encoding/json"
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
	if got := requests.Load(); got != 2 {
		t.Fatalf("%d requests after re-enrollment, want 2", got)
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
