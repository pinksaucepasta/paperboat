package machinecontrol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

func TestSourceRenewsWithExactMachineProofAndPersistsResult(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "runtime")
	store, err := identity.Open(identity.Config{StateRoot: root, Random: bytes.NewReader(bytes.Repeat([]byte{5}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{ServerURL: "https://unused.test", MachineID: "mch_1", EnvironmentID: "env_1", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 2, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	oldToken := strings.Repeat("o", 40)
	if err := store.SaveMachineControl(identity.MachineControl{MachineID: "mch_1", EnvironmentID: "env_1", InstallationGeneration: 2, Credential: oldToken, ExpiresAt: now.Add(time.Minute), KeyID: key.ID}); err != nil {
		t.Fatal(err)
	}
	newToken := strings.Repeat("n", 40)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machine-control-renewals" || r.Header.Get("Authorization") != "Bearer "+oldToken {
			t.Error("renewal request did not use the expected path and credential")
		}
		proof, proofErr := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if proofErr != nil || len(proof) == 0 {
			t.Error("missing machine proof")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"credential": newToken, "expires_at": now.Add(time.Hour)}})
	}))
	defer server.Close()
	source, err := NewSource(Config{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, Clock: func() time.Time { return now }, OperationID: func() (string, error) { return "operation-renew-1", nil }})
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token(t.Context())
	if err != nil || token != newToken {
		t.Fatalf("renewal token mismatch: err=%v", err)
	}
	persisted, err := store.MachineControl(now, 0)
	if err != nil || persisted.Credential != newToken {
		t.Fatalf("renewed credential was not persisted: err=%v", err)
	}
}

func TestSourceRejectsExpiredCredentialWithoutRenewalRequest(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "runtime")
	store, err := identity.Open(identity.Config{StateRoot: root, Random: bytes.NewReader(bytes.Repeat([]byte{6}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{ServerURL: "https://unused.test", MachineID: "mch_1", EnvironmentID: "env_1", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 2, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMachineControl(identity.MachineControl{MachineID: "mch_1", EnvironmentID: "env_1", InstallationGeneration: 2, Credential: strings.Repeat("e", 40), ExpiresAt: now.Add(-time.Second), KeyID: key.ID}); err != nil {
		t.Fatal(err)
	}
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()
	source, err := NewSource(Config{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, Clock: func() time.Time { return now }, OperationID: func() (string, error) { return "operation-expired-1", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(t.Context()); !errors.Is(err, ErrInvalid) {
		t.Fatal("expired machine-control credential was not rejected")
	}
	if requests != 0 {
		t.Fatalf("renewal requests = %d, want none", requests)
	}
}

func TestSourcePreservesProtectedStoreCauseAndRecovers(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "runtime-state")
	if err := os.WriteFile(root, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewSource(Config{ControlURL: "https://control.example", StateRoot: root, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"token": func() error { _, err := source.Token(t.Context()); return err },
		"proof": func() error {
			_, err := source.Proof(t.Context(), "operation-proof-1", http.MethodPost, "/v1/example", []byte(`{}`))
			return err
		},
		"initial token": func() error { _, err := source.EnsureInitial(t.Context()); return err },
	} {
		t.Run(name, func(t *testing.T) {
			failure := call()
			if !errors.Is(failure, ErrUnavailable) || errors.Is(failure, ErrInvalid) {
				t.Fatalf("failure=%v, want unavailable without authentication rejection", failure)
			}
			var pathErr *os.PathError
			if !errors.As(failure, &pathErr) || strings.Contains(failure.Error(), root) {
				t.Fatalf("protected-store cause/path contract failed: %T", failure)
			}
		})
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	store := saveMachineControlFixture(t, root, now, strings.Repeat("r", 40), now.Add(time.Hour))
	token, err := source.Token(t.Context())
	if err != nil || token != strings.Repeat("r", 40) {
		t.Fatalf("machine-control token did not recover: err=%v", err)
	}
	proof, err := source.Proof(t.Context(), "operation-proof-2", http.MethodPost, "/v1/example", []byte(`{}`))
	if err != nil || len(proof) == 0 {
		t.Fatalf("recovered proof length=%d err=%v", len(proof), err)
	}
	initial, err := source.EnsureInitial(t.Context())
	if err != nil || initial != token {
		t.Fatalf("initial credential did not reuse recovered token: err=%v", err)
	}
	if current, err := store.MachineControl(now, 0); err != nil || current.Credential != token {
		t.Fatalf("recovered credential was not persisted: err=%v", err)
	}
}

func TestSourceRenewalFailurePreservesHTTPAttemptAndRecovers(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "runtime")
	oldToken := strings.Repeat("o", 40)
	newToken := strings.Repeat("n", 40)
	saveMachineControlFixture(t, root, now, oldToken, now.Add(time.Minute))
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machine-control-renewals" || r.Header.Get("Authorization") != "Bearer "+oldToken {
			t.Error("renewal request did not use the expected path and credential")
		}
		if got := calls.Add(1); got == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "private-response-body")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"credential": newToken, "expires_at": now.Add(time.Hour)}})
	}))
	defer server.Close()
	source, err := NewSource(Config{
		ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport,
		Clock: func() time.Time { return now }, OperationID: func() (string, error) { return "operation-renew-1", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(t.Context()); err == nil {
		t.Fatal("503 renewal unexpectedly succeeded")
	} else {
		if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalid) || !errorreport.HTTPAttemptObserved(err) {
			t.Fatalf("renewal failure=%v, want unavailable and already-observed HTTP attempt", err)
		}
		var status interface{ DiagnosticStatus() int }
		if !errors.As(err, &status) || status.DiagnosticStatus() != http.StatusServiceUnavailable {
			t.Fatalf("renewal failure lost HTTP status: %T %v", err, err)
		}
		if strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "private-response-body") {
			t.Fatalf("renewal error exposed endpoint or response body: %v", err)
		}
	}
	if token, err := source.Token(t.Context()); err != nil || token != newToken {
		t.Fatalf("renewed credential was not reusable: err=%v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("renewal requests=%d, want failed attempt then recovery", calls.Load())
	}
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.MachineControl(now, 0)
	if err != nil || current.Credential != newToken {
		t.Fatalf("renewed credential was not persisted: err=%v", err)
	}
}

func TestSourceMapsOnlyExplicitRenewalDenialsToInvalidCredential(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		status      int
		wantInvalid bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantInvalid: true},
		{name: "forbidden", status: http.StatusForbidden, wantInvalid: true},
		{name: "unexpected client status", status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "runtime")
			saveMachineControlFixture(t, root, now, strings.Repeat("o", 40), now.Add(time.Minute))
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			source, err := NewSource(Config{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, Clock: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			_, err = source.Token(t.Context())
			if test.wantInvalid {
				if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrUnavailable) {
					t.Fatalf("HTTP %d renewal error=%v, want finite invalid-credential rejection", test.status, err)
				}
				return
			}
			if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalid) {
				t.Fatalf("HTTP %d renewal error=%v, want unavailable rather than credential rejection", test.status, err)
			}
		})
	}
}

func TestSourceMixedUnauthorizedBodyCloseFailureIsUnavailable(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "runtime")
	saveMachineControlFixture(t, root, now, strings.Repeat("o", 40), now.Add(time.Minute))
	closeFailure := errors.New("response body close failed")
	source, err := NewSource(Config{
		ControlURL: "https://control.example", StateRoot: root,
		Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       closeFailureBody{Reader: strings.NewReader("rejected"), err: closeFailure},
			}, nil
		}),
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal("could not construct machine-control source")
	}
	if _, err := source.Token(t.Context()); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalid) || !errors.Is(err, closeFailure) {
		t.Fatal("mixed unauthorized response and body-close failure was not retained as unavailable")
	}
}

func TestSourceRedirectIsOperationalNotCredentialRejection(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "runtime")
	saveMachineControlFixture(t, root, now, strings.Repeat("o", 40), now.Add(time.Minute))
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	defer server.Close()
	source, err := NewSource(Config{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(t.Context()); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalid) || !errors.Is(err, errControlRedirect) {
		t.Fatalf("redirect failure=%v, want unavailable with preserved redirect cause and no auth rejection", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("redirect requests=%d, want only the authenticated renewal request", requests.Load())
	}
}

func saveMachineControlFixture(t *testing.T, root string, now time.Time, credential string, expiresAt time.Time) *identity.Store {
	t.Helper()
	store, err := identity.Open(identity.Config{StateRoot: root, Random: bytes.NewReader(bytes.Repeat([]byte{5}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{
		ServerURL: "https://control.example", MachineID: "machine_1", EnvironmentID: "environment_1",
		PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()),
		InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 3, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMachineControl(identity.MachineControl{
		MachineID: "machine_1", EnvironmentID: "environment_1", InstallationGeneration: 3,
		Credential: credential, ExpiresAt: expiresAt, KeyID: key.ID,
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (roundTripper roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTripper(request)
}

type closeFailureBody struct {
	*strings.Reader
	err error
}

func (body closeFailureBody) Close() error { return body.err }

func TestEnsureInitialUsesHelperIdentityAndRetriesWithStableOperation(t *testing.T) {
	now := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "runtime")
	const helperToken = "helper-identity-credential-012345678901234567890"
	const machineToken = "machine-control-credential-012345678901234567890"
	const rotatedMachineToken = "machine-control-credential-rotated-012345678901234567890"
	helperExpiresAt := time.Now().UTC().Add(24 * time.Hour)
	var operations []string
	var machineControlCalls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/helper-enrollments":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "mch_1", "environment_id": "env_1", "credential": helperToken, "expires_at": helperExpiresAt}})
		case "/v1/machine-control-credentials":
			machineControlCalls++
			if r.Header.Get("Authorization") != "Bearer "+helperToken {
				t.Error("initial request did not use the expected helper credential")
			}
			proof, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Payload string `json:"payload"`
			}
			if err := json.Unmarshal(proof, &envelope); err != nil {
				t.Fatal(err)
			}
			payload, err := base64.RawURLEncoding.DecodeString(envelope.Payload)
			if err != nil {
				t.Fatal(err)
			}
			var claims struct {
				HelperID    string `json:"helper_id"`
				OperationID string `json:"operation_id"`
			}
			if err := json.Unmarshal(payload, &claims); err != nil || claims.HelperID != "hlp_1" {
				t.Fatalf("initial proof did not contain the expected helper identity: err=%v", err)
			}
			operations = append(operations, claims.OperationID)
			credential := machineToken
			if machineControlCalls > 2 {
				credential = rotatedMachineToken
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"credential": credential, "expires_at": now.Add(time.Minute)}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := enrollment.NewClient(server.Client().Transport, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Enroll(t.Context(), enrollment.Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: strings.Repeat("e", 32)}); err != nil {
		t.Fatal(err)
	}
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{ServerURL: server.URL, MachineID: "mch_1", EnvironmentID: "env_1", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	source, err := NewSource(Config{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if token, err := source.EnsureInitial(t.Context()); err != nil || token != machineToken {
		t.Fatalf("initial credential was not returned: err=%v", err)
	}
	if err := os.Remove(filepath.Join(root, "machine-control.json")); err != nil {
		t.Fatal(err)
	}
	if token, err := source.EnsureInitial(t.Context()); err != nil || token != machineToken {
		t.Fatalf("initial credential was not reused on retry: err=%v", err)
	}
	if err := os.Remove(filepath.Join(root, "machine-control.json")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if token, err := source.EnsureInitial(t.Context()); err != nil || token != rotatedMachineToken {
		t.Fatalf("expired initial credential did not rotate: err=%v", err)
	}
	if len(operations) != 3 || operations[0] == "" || operations[0] != operations[1] || operations[1] != operations[2] {
		t.Fatalf("initial operations=%q", operations)
	}
	persisted, err := store.MachineControl(now, 0)
	if err != nil || persisted.Credential != rotatedMachineToken {
		t.Fatalf("rotated credential was not persisted: err=%v", err)
	}
}
