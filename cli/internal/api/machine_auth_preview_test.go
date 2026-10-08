package api

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

type machineAuthTestSource struct{}

func (machineAuthTestSource) Token(context.Context) (string, error) { return "machine-token", nil }

func (machineAuthTestSource) Proof(_ context.Context, operationID, method, path string, body []byte) ([]byte, error) {
	return []byte(strings.Join([]string{operationID, method, path, string(body)}, "|")), nil
}

func TestClientMachineAuthBindsMutationAndUsesMachineIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/previews" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("Authorization") != "Bearer machine-token" || r.Header.Get("X-Paperboat-Machine-Identity") != "machine-token" {
			t.Fatalf("machine auth headers = %#v", r.Header)
		}
		want := "create-op-01|POST|/v1/previews|" + string(body)
		got, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if err != nil || string(got) != want {
			t.Fatalf("proof = %q, want %q, err=%v", got, want, err)
		}
		if r.Header.Get("Idempotency-Key") != "create-op-01" {
			t.Fatalf("idempotency key = %q", r.Header.Get("Idempotency-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"schema":"paperboat.preview-tunnel/v1","kind":"operation","id":"op_01","resource_kind":"preview_lease","resource_id":"prv_01","phase":"connecting","state":"running","progress":1,"retrying":false,"correlation_id":"correlation_01","created_at":"2026-08-30T12:00:00Z","updated_at":"2026-08-30T12:00:00Z"}}`)
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "cli-session"}, server.Client())
	client.SetMachineAuth(machineAuthTestSource{})
	var out PreviewLeaseOperation
	if err := client.doRequestMeta(context.Background(), http.MethodPost, "/v1/previews", map[string]string{"owner": "machine_01"}, &out, http.Header{"Idempotency-Key": []string{"create-op-01"}}, false, nil); err != nil {
		t.Fatal(err)
	}
	if out.ID != "op_01" {
		t.Fatalf("operation = %#v", out)
	}
}

func TestClientMachineAuthRequiresMutationIdempotencyKey(t *testing.T) {
	client := New("https://api.example.test", config.Credential{}, nil)
	client.SetMachineAuth(machineAuthTestSource{})
	var out map[string]any
	if err := client.doRequestMeta(context.Background(), http.MethodPost, "/v1/previews", map[string]string{"owner": "machine_01"}, &out, nil, false, nil); err == nil || !strings.Contains(err.Error(), "Idempotency-Key") {
		t.Fatalf("error = %v, want idempotency-key rejection", err)
	}
}

func TestCreatePreviewLeaseMachineMutationThenClientRead(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	lease := PreviewLease{
		Schema: PreviewTunnelSchemaV1, Kind: "preview_lease", ID: "prv_01", AccountID: "acct_01", ActorID: "actor_01",
		OwnerMachineID: "machine_01", OwnerSessionID: "session_01", OwnerSessionKind: "foreground", Target: PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"},
		AccessMode: "public", Endpoint: "https://quiet-river-7.preview.example.test", LeaseDeadline: now.Add(time.Hour),
		State: "connecting", AllocationState: "pending", EdgeState: "pending", OriginState: "unknown", CreatedAt: now, LastRenewedAt: now,
	}
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/previews":
			if r.Header.Get("Authorization") != "Bearer machine-token" || r.Header.Get("X-Paperboat-Machine-Identity") != "machine-token" {
				t.Fatalf("mutation authorization = %q identity = %q", r.Header.Get("Authorization"), r.Header.Get("X-Paperboat-Machine-Identity"))
			}
			if r.Header.Get("X-Paperboat-Machine-Proof") == "" || r.Header.Get("Idempotency-Key") != "create-op-01" {
				t.Fatalf("mutation headers = %#v", r.Header)
			}
			w.Header().Set("X-Paperboat-Operation-ID", "op_01")
			writePreviewLeaseEnvelope(t, w, PreviewLeaseOperation{
				Schema: PreviewTunnelSchemaV1, Kind: "operation", ID: "op_01", ResourceKind: "preview_lease", ResourceID: "prv_01",
				Phase: "connecting", State: "running", Progress: 1, CorrelationID: "correlation_01", CreatedAt: now, UpdatedAt: now,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/previews/prv_01":
			if r.Header.Get("Authorization") != "Bearer cli-session" {
				t.Fatalf("read authorization = %q", r.Header.Get("Authorization"))
			}
			if r.Header.Get("X-Paperboat-Machine-Identity") != "" || r.Header.Get("X-Paperboat-Machine-Proof") != "" {
				t.Fatalf("machine auth leaked into read headers = %#v", r.Header)
			}
			w.Header().Set("ETag", `"ptv1:preview_lease:cHJ2XzAx:1"`)
			writePreviewLeaseEnvelope(t, w, lease)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.URL, config.Credential{AccessToken: "cli-session"}, server.Client())
	client.SetMachineAuth(machineAuthTestSource{})
	created, err := client.CreatePreviewLease(context.Background(), PreviewLeaseCreateRequest{
		OwnerMachineID: "machine_01", OwnerSessionID: "session_01", OwnerSessionKind: "foreground", Target: lease.Target,
	}, "create-op-01")
	if err != nil {
		t.Fatal(err)
	}
	if created.CreateOperationID != "op_01" || created.ETag != `"ptv1:preview_lease:cHJ2XzAx:1"` {
		t.Fatalf("created lease = %#v", created)
	}
	if got := strings.Join(methods, ","); got != "POST /v1/previews,GET /v1/previews/prv_01" {
		t.Fatalf("methods = %q", got)
	}
}

func TestCreatePreviewLeaseMachineMutationWithoutClientReadCredentialFailsClosed(t *testing.T) {
	var reads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads++
		}
		w.Header().Set("X-Paperboat-Operation-ID", "op_01")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": PreviewLeaseOperation{
			Schema: PreviewTunnelSchemaV1, Kind: "operation", ID: "op_01", ResourceKind: "preview_lease", ResourceID: "prv_01",
			Phase: "connecting", State: "running", Progress: 1, CorrelationID: "correlation_01", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	client.SetMachineAuth(machineAuthTestSource{})
	_, err := client.CreatePreviewLease(context.Background(), PreviewLeaseCreateRequest{
		OwnerMachineID: "machine_01", OwnerSessionID: "session_01", OwnerSessionKind: "foreground", Target: PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"},
	}, "create-op-01")
	if err != ErrMachineAuthReadRequiresClientSession {
		t.Fatalf("error = %v, want %v", err, ErrMachineAuthReadRequiresClientSession)
	}
	if reads != 0 {
		t.Fatalf("GET requests = %d, want 0", reads)
	}
}

type canonicalMachineAuthTestSource struct{ store *identity.Store }

func (s canonicalMachineAuthTestSource) Token(context.Context) (string, error) {
	return "machine-token", nil
}
func (s canonicalMachineAuthTestSource) Proof(_ context.Context, operation, method, path string, body []byte) ([]byte, error) {
	return s.store.MachineProof(operation, method, path, body, time.Now().UTC())
}

// Exercise the server's canonical URL.Path and exact body-hash boundary with
// the production Ed25519 signer, including workspace tamper rejection.
func TestScopedMachineProofUsesCanonicalPathAndSignsWorkspaceBody(t *testing.T) {
	root := filepath.Join(t.TempDir(), "identity")
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{ServerURL: "https://api.example.test", MachineID: "machine_1", EnvironmentID: "machine_1", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/previews", "/v1/tunnels"} {
		t.Run(path, func(t *testing.T) {
			var checked atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
				if err != nil {
					t.Error(err)
					w.WriteHeader(401)
					return
				}
				var envelope struct {
					Algorithm string `json:"alg"`
					Payload   string `json:"payload"`
					Signature string `json:"signature"`
				}
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Error(err)
					w.WriteHeader(401)
					return
				}
				payload, payloadErr := base64.RawURLEncoding.DecodeString(envelope.Payload)
				signature, signatureErr := base64.RawURLEncoding.DecodeString(envelope.Signature)
				if payloadErr != nil || signatureErr != nil || envelope.Algorithm != "EdDSA" || !ed25519.Verify(key.Public(), payload, signature) {
					t.Error("invalid real machine signature")
					w.WriteHeader(401)
					return
				}
				var claims struct {
					Method     string `json:"method"`
					Path       string `json:"path"`
					BodySHA256 string `json:"body_sha256"`
				}
				if err := json.Unmarshal(payload, &claims); err != nil {
					t.Error(err)
					w.WriteHeader(401)
					return
				}
				digest := sha256.Sum256(body)
				if claims.Method != r.Method || claims.Path != r.URL.Path || claims.BodySHA256 != base64.RawURLEncoding.EncodeToString(digest[:]) {
					t.Errorf("canonical server proof mismatch: method=%q path=%q want=%q", claims.Method, claims.Path, r.URL.Path)
					w.WriteHeader(401)
					return
				}
				var input struct {
					Workspace string `json:"workspace"`
				}
				if err := json.Unmarshal(body, &input); err != nil || input.Workspace != "team-a" || input.Workspace != r.URL.Query().Get("workspace") {
					t.Errorf("signed body/query workspace mismatch: %q", input.Workspace)
					w.WriteHeader(400)
					return
				}
				changedDigest := sha256.Sum256([]byte(strings.Replace(string(body), "team-a", "team-b", 1)))
				if claims.BodySHA256 == base64.RawURLEncoding.EncodeToString(changedDigest[:]) {
					t.Error("workspace mutation retained signed body authority")
				}
				checked.Store(true)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":{"accepted":true}}`)
			}))
			defer server.Close()
			client := New(server.URL, config.Credential{}, server.Client())
			if err := client.SetWorkspace("team-a"); err != nil {
				t.Fatal(err)
			}
			client.SetMachineAuth(canonicalMachineAuthTestSource{store})
			body := map[string]string{"workspace": "team-a"}
			headers := http.Header{"Idempotency-Key": []string{"workspace-operation"}}
			var err error
			if path == "/v1/tunnels" {
				err = client.doTunnelRequest(context.Background(), http.MethodPost, path, body, nil, headers, nil)
			} else {
				err = client.doRequestMeta(context.Background(), http.MethodPost, path, body, nil, headers, false, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !checked.Load() {
				t.Fatal("proof verifier was not reached")
			}
		})
	}
}
