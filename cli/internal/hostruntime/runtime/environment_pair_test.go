//go:build darwin || linux

package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	identity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPairEnvironmentRegistersAuthenticatedRecipientBeforeReadiness(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	for _, outcome := range []string{"empty", "pending", "foreign", "registration_denied"} {
		t.Run(outcome, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "runtime")
			var public ed25519.PublicKey
			var recipient api.VaultLayerRecipient
			registrations, contexts := 0, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if r.URL.Path == "/v1/helper-enrollments" {
					var input struct {
						PublicKey string `json:"public_key"`
					}
					if json.Unmarshal(body, &input) != nil {
						t.Error("invalid enrollment")
						return
					}
					public, _ = base64.RawURLEncoding.DecodeString(input.PublicKey)
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "helper_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": "identity-credential-0123456789012345", "expires_at": time.Now().Add(time.Hour)}})
					return
				}
				rawProof, _ := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
				var proof struct {
					Algorithm string `json:"alg"`
					Payload   string `json:"payload"`
					Signature string `json:"signature"`
				}
				if json.Unmarshal(rawProof, &proof) != nil {
					t.Error("missing machine proof")
					return
				}
				payload, _ := base64.RawURLEncoding.DecodeString(proof.Payload)
				signature, _ := base64.RawURLEncoding.DecodeString(proof.Signature)
				var claims struct {
					HelperID      string `json:"helper_id"`
					EnvironmentID string `json:"environment_id"`
					OperationID   string `json:"operation_id"`
					Method        string `json:"method"`
					Path          string `json:"path"`
					BodySHA256    string `json:"body_sha256"`
				}
				digest := sha256.Sum256(body)
				if proof.Algorithm != "EdDSA" || !ed25519.Verify(public, payload, signature) || json.Unmarshal(payload, &claims) != nil || claims.HelperID != "helper_1" || claims.EnvironmentID != "env_1" || claims.OperationID != r.Header.Get("Idempotency-Key") || claims.Method != r.Method || claims.Path != r.URL.Path || claims.BodySHA256 != base64.RawURLEncoding.EncodeToString(digest[:]) || r.Header.Get("Authorization") != "Bearer identity-credential-0123456789012345" {
					t.Error("pair request not bound to enrolled machine signature and exact body")
					return
				}
				switch r.URL.Path {
				case "/v1/environment/hosts/machine_1/key":
					registrations++
					if outcome == "registration_denied" {
						w.WriteHeader(403)
						return
					}
					var input struct {
						InstallationGeneration uint64 `json:"installation_generation"`
						HostKeyGeneration      uint64 `json:"host_key_generation"`
						HostPublic             string `json:"host_public"`
					}
					if json.Unmarshal(body, &input) != nil || input.InstallationGeneration != 1 || input.HostKeyGeneration != 1 {
						t.Error("incorrect recipient generations")
						return
					}
					recipient = api.VaultLayerRecipient{RecipientAccount: "account_1", MachineID: "machine_1", InstallationGeneration: input.InstallationGeneration, HostKeyGeneration: input.HostKeyGeneration, HostPublic: input.HostPublic}
					json.NewEncoder(w).Encode(map[string]any{"data": recipient})
				case "/v1/environment/hosts/machine_1/layers":
					contexts++
					if registrations != 1 {
						t.Error("delivery checked before recipient registration")
					}
					var binding struct {
						WorkspaceID    string `json:"workspace_id"`
						ActorAccountID string `json:"actor_account_id"`
					}
					if json.Unmarshal(body, &binding) != nil || binding.WorkspaceID != "personal" || binding.ActorAccountID != "account_1" {
						t.Error("wrong readiness authorization")
						return
					}
					bundle := api.VaultLayerContext{WorkspaceID: "personal", ActorAccountID: "account_1", MachineID: "machine_1", Recipient: recipient, Layers: []api.VaultLayerDelivery{}}
					if outcome == "pending" {
						bundle.Layers = []api.VaultLayerDelivery{{Source: api.VaultLayerSource{VaultLayerCoordinate: api.VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account_1"}}, Recipient: recipient, State: "pending"}}
					}
					if outcome == "foreign" {
						bundle.ActorAccountID = "foreign_account"
					}
					json.NewEncoder(w).Encode(map[string]any{"data": bundle})
				default:
					t.Error("unexpected pairing request")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client, err := enrollment.NewClient(server.Client().Transport, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Enroll(context.Background(), enrollment.Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: strings.Repeat("g", 32)}); err != nil {
				t.Fatal(err)
			}
			store, err := identity.Open(identity.Config{StateRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			key := store.Current()
			registration := identity.Registration{ServerURL: server.URL, AccountID: "account_1", MachineID: "machine_1", EnvironmentID: "env_1", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, UpdatedAt: time.Now()}
			if err := store.SaveRegistration(registration); err != nil {
				t.Fatal(err)
			}
			err = PreparePairEnvironment(context.Background(), root, server.URL, server.Client().Transport)
			if (outcome == "empty") != (err == nil) {
				t.Fatalf("readiness outcome=%s err=%v", outcome, err)
			}
			if registrations != 1 || outcome == "registration_denied" && contexts != 0 || outcome != "registration_denied" && contexts != 1 {
				t.Fatalf("registration=%d context=%d", registrations, contexts)
			}
			keys, err := productionEnvironmentKeySourceForState(root, registration)
			if err != nil {
				t.Fatal(err)
			}
			material, err := keys.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer material.Destroy()
			retainedPublic, err := material.Public()
			if err != nil {
				t.Fatal(err)
			}
			if outcome != "registration_denied" && recipient.HostPublic != base64.RawURLEncoding.EncodeToString(retainedPublic[:]) {
				t.Fatal("registered key differs from retained custody")
			}
		})
	}
}
