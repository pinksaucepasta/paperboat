package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type renewalMetric struct {
	mu    sync.Mutex
	count float64
}

func (m *renewalMetric) Record(name string, value float64, labels map[string]string) error {
	if name == "paperboat_runtime_renewal_failures_total" && len(labels) == 0 {
		m.mu.Lock()
		m.count += value
		m.mu.Unlock()
	}
	return nil
}

func TestRenewingTokenSourceRenewsAndPersistsIdentity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	renewedCredential := "renewed-helper-credential-012345678901234567890"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/helper-enrollments":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": "initial-helper-credential-012345678901234567890", "expires_at": now.Add(5 * time.Minute)}})
		case "/v1/helper-identity-renewals":
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
				t.Errorf("renew headers=%v", r.Header)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": renewedCredential, "expires_at": now.Add(time.Hour)}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	if err := makeEnrollmentStateRootPrivateForTest(root); err != nil {
		t.Fatal("protect enrollment test state root")
	}
	client, _ := NewClient(server.Client().Transport, time.Second)
	if _, err := client.Enroll(context.Background(), Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: "grant-credential-012345678901234567890"}); err != nil {
		t.Fatal(err)
	}
	source, err := NewRenewingTokenSource(RenewingTokenConfig{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, RenewBefore: 10 * time.Minute, Timeout: time.Second, Clock: func() time.Time { return now }, OperationID: func() (string, error) { return "op_renew_0001", nil }})
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != renewedCredential {
		t.Fatalf("token=%q", token)
	}
	stored, err := LoadRuntimeIdentity(root, now)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Credential != renewedCredential || stored.KeyID == "" {
		t.Fatalf("stored=%#v", stored)
	}
}

func TestRenewingTokenSourcesSerializePersistedRenewalAndHonorCancellation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	const renewedCredential = "renewed-helper-credential-012345678901234567890"
	var renewalCount atomic.Int32
	var blockNextRenewal atomic.Bool
	renewalRequests := make(chan struct{}, 4)
	blockedRenewal := make(chan struct{}, 1)
	releaseBlockedRenewal := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/helper-enrollments":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": "initial-helper-credential-012345678901234567890", "expires_at": now.Add(5 * time.Minute)}})
		case "/v1/helper-identity-renewals":
			renewalCount.Add(1)
			renewalRequests <- struct{}{}
			if blockNextRenewal.CompareAndSwap(true, false) {
				blockedRenewal <- struct{}{}
				select {
				case <-releaseBlockedRenewal:
				case <-r.Context().Done():
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": renewedCredential, "expires_at": now.Add(time.Hour)}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	if err := makeEnrollmentStateRootPrivateForTest(root); err != nil {
		t.Fatal("protect enrollment test state root")
	}
	client, err := NewClient(server.Client().Transport, time.Second)
	if err != nil {
		t.Fatal("construct enrollment client")
	}
	if _, err := client.Enroll(context.Background(), Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: "grant-credential-012345678901234567890"}); err != nil {
		t.Fatal("enroll renewal fixture")
	}
	newSourceWithTimeout := func(id string, timeout time.Duration) *RenewingTokenSource {
		t.Helper()
		source, err := NewRenewingTokenSource(RenewingTokenConfig{
			ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport,
			RenewBefore: 10 * time.Minute, Timeout: timeout, Clock: func() time.Time { return now },
			OperationID: func() (string, error) { return id, nil },
		})
		if err != nil {
			t.Fatal("construct renewing token source")
		}
		return source
	}
	newSource := func(id string) *RenewingTokenSource { return newSourceWithTimeout(id, time.Second) }

	// Hold the real OS lock while two independent sources reach Token. Neither
	// may issue a renewal until the lock is released; after the first persists,
	// the second must reload and reuse the new token.
	heldLock, err := openRuntimeIdentityRenewalLock(root)
	if err != nil {
		t.Fatal("open renewal lock")
	}
	locked, err := heldLock.Try()
	if err != nil || !locked {
		_ = heldLock.Close()
		t.Fatalf("hold renewal lock: locked=%v err=%v", locked, err)
	}
	results := make(chan struct {
		token string
		err   error
	}, 2)
	started := make(chan struct{}, 2)
	for _, source := range []*RenewingTokenSource{newSource("op_renew_race_a"), newSource("op_renew_race_b")} {
		go func(source *RenewingTokenSource) {
			started <- struct{}{}
			token, err := source.Token(context.Background())
			results <- struct {
				token string
				err   error
			}{token, err}
		}(source)
	}
	<-started
	<-started
	select {
	case <-renewalRequests:
		_ = heldLock.Close()
		t.Fatal("renewal request passed an account lock held by another source")
	case <-time.After(150 * time.Millisecond):
	}
	if err := heldLock.Close(); err != nil {
		t.Fatal("release held renewal lock")
	}
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil || result.token != renewedCredential {
			t.Fatalf("concurrent token result token=%q err=%v", result.token, result.err)
		}
	}
	if got := renewalCount.Load(); got != 1 {
		t.Fatalf("renewal requests=%d, want one shared renewal", got)
	}
	stored, err := LoadRuntimeIdentity(root, now)
	if err != nil || stored.Credential != renewedCredential {
		t.Fatalf("persisted identity credential=%q err=%v", stored.Credential, err)
	}

	// Restore a near-expiry credential, hold the same file lock, and cancel a
	// second source while it waits. Cancellation must release its file handle
	// without sending another request or altering the stored identity.
	stored.ExpiresAt = now.Add(5 * time.Minute)
	if err := writeIdentity(root, stored); err != nil {
		t.Fatal("restore near-expiry identity")
	}
	heldLock, err = openRuntimeIdentityRenewalLock(root)
	if err != nil {
		t.Fatal("reopen renewal lock")
	}
	locked, err = heldLock.Try()
	if err != nil || !locked {
		_ = heldLock.Close()
		t.Fatalf("reacquire renewal lock: locked=%v err=%v", locked, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	cancelSource := newSource("op_renew_cancel")
	go func() {
		_, err := cancelSource.Token(ctx)
		canceled <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) {
			_ = heldLock.Close()
			t.Fatalf("canceled lock wait error=%v", err)
		}
	case <-time.After(time.Second):
		_ = heldLock.Close()
		t.Fatal("renewal lock wait ignored context cancellation")
	}
	if err := heldLock.Close(); err != nil {
		t.Fatal("release cancellation test lock")
	}
	if got := renewalCount.Load(); got != 1 {
		t.Fatalf("canceled wait sent a renewal request; count=%d", got)
	}

	// Config.Timeout bounds file-lock acquisition even if the caller supplies
	// an unbounded context.
	heldLock, err = openRuntimeIdentityRenewalLock(root)
	if err != nil {
		t.Fatal("reopen renewal lock for timeout check")
	}
	locked, err = heldLock.Try()
	if err != nil || !locked {
		_ = heldLock.Close()
		t.Fatalf("hold renewal lock for timeout check: locked=%v err=%v", locked, err)
	}
	_, err = newSourceWithTimeout("op_renew_timeout", 100*time.Millisecond).Token(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrUnavailable) {
		_ = heldLock.Close()
		t.Fatalf("configured lock-wait timeout error=%v", err)
	}
	if err := heldLock.Close(); err != nil {
		t.Fatal("release timeout test lock")
	}
	if got := renewalCount.Load(); got != 1 {
		t.Fatalf("timed-out wait sent a renewal request; count=%d", got)
	}

	// A second call through the same source also observes its caller's deadline
	// while the first call holds the account lock during a blocked endpoint.
	gateSource := newSource("op_renew_same_source_first")
	blockNextRenewal.Store(true)
	firstResult := make(chan error, 1)
	go func() {
		_, err := gateSource.Token(context.Background())
		firstResult <- err
	}()
	select {
	case <-blockedRenewal:
	case <-time.After(time.Second):
		t.Fatal("first renewal did not reach the endpoint")
	}
	gateCtx, gateCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	secondResult := make(chan error, 1)
	go func() {
		_, err := gateSource.Token(gateCtx)
		secondResult <- err
	}()
	select {
	case err := <-secondResult:
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrUnavailable) {
			close(releaseBlockedRenewal)
			gateCancel()
			t.Fatalf("same-source canceled wait error=%v", err)
		}
	case <-time.After(time.Second):
		close(releaseBlockedRenewal)
		gateCancel()
		<-firstResult
		t.Fatal("same-source token wait ignored its deadline")
	}
	gateCancel()
	close(releaseBlockedRenewal)
	if err := <-firstResult; err != nil {
		t.Fatalf("first same-source renewal failed: %v", err)
	}
}

func TestRenewingTokenSourceRecoversIdentityAfterLongOutage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/helper-enrollments":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": "initial-helper-credential-012345678901234567890", "expires_at": now.Add(time.Hour)}})
		case "/v1/helper-identity-renewals":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": "renewed-helper-credential-012345678901234567890", "expires_at": now.Add(time.Hour)}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	if err := makeEnrollmentStateRootPrivateForTest(root); err != nil {
		t.Fatal("protect enrollment test state root")
	}
	client, _ := NewClient(server.Client().Transport, time.Second)
	if _, err := client.Enroll(context.Background(), Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: "grant-credential-012345678901234567890"}); err != nil {
		t.Fatal(err)
	}
	var identity RuntimeIdentity
	body, _ := os.ReadFile(filepath.Join(root, "runtime-identity.json"))
	if err := json.Unmarshal(body, &identity); err != nil {
		t.Fatal(err)
	}
	identity.ExpiresAt = now.Add(-30 * 24 * time.Hour)
	if err := writeIdentity(root, identity); err != nil {
		t.Fatal(err)
	}
	source, err := NewRenewingTokenSource(RenewingTokenConfig{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, Clock: func() time.Time { return now }, OperationID: func() (string, error) { return "op_renew_expired", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("renew expired identity: %v", err)
	}
}

func TestRenewingTokenSourceRecordsRenewalFailure(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root := t.TempDir()
	if err := writeIdentity(root, RuntimeIdentity{Version: 1, HelperID: "hlp_1", MachineID: "machine_1", EnvironmentID: "env_1", Credential: "expired-helper-credential-012345678901234567890", ExpiresAt: now.Add(-time.Hour), KeyID: "ed25519:test"}); err != nil {
		t.Fatal(err)
	}
	metric := &renewalMetric{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	source, err := NewRenewingTokenSource(RenewingTokenConfig{ControlURL: "https://127.0.0.1:1", StateRoot: root, Transport: transport, Timeout: time.Millisecond, Clock: func() time.Time { return now }, OperationID: func() (string, error) { return "op_renew_failure", nil }, Metrics: metric})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background()); err == nil {
		t.Fatal("renewal unexpectedly succeeded")
	}
	metric.mu.Lock()
	defer metric.mu.Unlock()
	if metric.count != 1 {
		t.Fatalf("renewal failure metric=%v", metric.count)
	}
}

// A revoked machine's renewal is refused with 401: the error is that observed refusal, so the daemon says
// signed out at once instead of "control plane unavailable" for ~45 s (C036).
func TestRenewingTokenSourceKeepsTheRefusalOfARevokedMachine(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/helper-enrollments":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": "initial-helper-credential-012345678901234567890", "expires_at": now.Add(5 * time.Minute)}})
		case "/v1/helper-identity-renewals":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "machine_revoked", "message": "revoked"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	if err := makeEnrollmentStateRootPrivateForTest(root); err != nil {
		t.Fatal("protect enrollment test state root")
	}
	client, _ := NewClient(server.Client().Transport, time.Second)
	if _, err := client.Enroll(context.Background(), Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: "grant-credential-012345678901234567890"}); err != nil {
		t.Fatal(err)
	}
	source, err := NewRenewingTokenSource(RenewingTokenConfig{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, RenewBefore: 10 * time.Minute, Timeout: time.Second, Clock: func() time.Time { return now }, OperationID: func() (string, error) { return "op_renew_0002", nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.Token(context.Background())
	fault := errorreport.ProjectFault(context.Background(), "paperboatd", "machine_control", "control_request", "control_request_failed", err)
	if err == nil || fault.HTTPStatus != http.StatusUnauthorized || fault.Outcome != "rejected" {
		t.Fatalf("refused renewal err=%v status=%d outcome=%q", err, fault.HTTPStatus, fault.Outcome)
	}
}
