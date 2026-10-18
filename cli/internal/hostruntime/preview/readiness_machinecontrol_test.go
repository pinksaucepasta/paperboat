package preview

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machinecontrol"
)

func TestReadinessPublicationUsesProductionMachineControlProofs(t *testing.T) {
	now := time.Now().UTC()
	root := filepath.Join(t.TempDir(), "identity")
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{ServerURL: "https://control.example.test", MachineID: "machine_1", EnvironmentID: "environment_1", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 3, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	credential := strings.Repeat("c", 40)
	if err := store.SaveMachineControl(identity.MachineControl{MachineID: "machine_1", EnvironmentID: "environment_1", InstallationGeneration: 3, Credential: credential, ExpiresAt: now.Add(time.Hour), KeyID: key.ID}); err != nil {
		t.Fatal(err)
	}
	lease := testDispatchRequest(t, now).Lease()
	var mu sync.Mutex
	var methods []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/previews/prv_dispatch_1/readiness" || r.Header.Get("Authorization") != "Bearer "+credential || r.Header.Get("X-Paperboat-Machine-Identity") != credential {
			t.Error("readiness used the wrong resource or machine identity")
		}
		verifyReadinessProductionProof(t, key.Public(), r, body, now)
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		observed := sessionReadyLease(lease)
		if r.Method == http.MethodPost {
			observed.State, observed.EdgeState = "connecting", "pending"
		} else if r.Method != http.MethodGet || len(body) != 0 || r.Header.Get("If-Match") != "" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("publication GET was not an empty, independently signed read")
		}
		w.Header().Set("ETag", formatLeaseETag(lease.ID, 2))
		_ = json.NewEncoder(w).Encode(map[string]any{"data": leaseToAPI(observed)})
	}))
	defer server.Close()
	source, err := machinecontrol.NewSource(machinecontrol.Config{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodHead, http.MethodPatch} {
		if _, err := source.Proof(t.Context(), "operation_dispatch_1", method, "/v1/previews/prv_dispatch_1/readiness", nil); !errors.Is(err, machinecontrol.ErrInvalid) {
			t.Fatalf("unsupported method %s was accepted", method)
		}
	}
	endpoint, _ := url.Parse(server.URL)
	observer, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{ControlURL: server.URL, AllowedHosts: []string{endpoint.Hostname()}, Identities: source, Proofs: source, Transport: server.Client().Transport})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := observer.ObservePreviewReadiness(t.Context(), DispatchReadiness{OperationID: "operation_dispatch_1", IdempotencyKey: "create-key", RequestID: "request_dispatch_1", CorrelationID: "correlation_dispatch_1"}, lease, 1)
	if err != nil || !isReadyLease(ready) {
		t.Fatalf("production machine signer could not complete pending publication: %v", err)
	}
	mu.Lock()
	got := strings.Join(methods, ",")
	mu.Unlock()
	if got != "POST,GET" {
		t.Fatalf("publication methods=%s, want one POST then signed GET", got)
	}
}

func verifyReadinessProductionProof(t *testing.T, public ed25519.PublicKey, r *http.Request, body []byte, now time.Time) {
	t.Helper()
	encoded, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
	if err != nil {
		t.Error("invalid proof encoding")
		return
	}
	var envelope struct {
		Algorithm          string `json:"alg"`
		Payload, Signature string
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Error("invalid proof envelope")
		return
	}
	payload, payloadErr := base64.RawURLEncoding.DecodeString(envelope.Payload)
	signature, signatureErr := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if payloadErr != nil || signatureErr != nil || envelope.Algorithm != "EdDSA" || !ed25519.Verify(public, payload, signature) {
		t.Error("machine proof signature did not verify")
		return
	}
	var claims struct {
		MachineID              string    `json:"machine_id"`
		EnvironmentID          string    `json:"environment_id"`
		InstallationGeneration int64     `json:"installation_generation"`
		OperationID            string    `json:"operation_id"`
		Method                 string    `json:"method"`
		Path                   string    `json:"path"`
		BodySHA256             string    `json:"body_sha256"`
		IssuedAt               time.Time `json:"issued_at"`
		ExpiresAt              time.Time `json:"expires_at"`
	}
	digest := sha256.Sum256(body)
	if err := json.Unmarshal(payload, &claims); err != nil || claims.MachineID != "machine_1" || claims.EnvironmentID != "environment_1" || claims.InstallationGeneration != 3 || claims.OperationID != "operation_dispatch_1" || claims.Method != r.Method || claims.Path != r.URL.Path || claims.BodySHA256 != base64.RawURLEncoding.EncodeToString(digest[:]) || !claims.IssuedAt.Equal(now) || !claims.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Error("proof did not bind the exact method/path/body and current installation")
	}
}
