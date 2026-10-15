package hostruntimecmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
)

// Recovery must use current account custody rather than replaying the old
// token snapshot retained by the protected installation journal.
func TestAdmittedResumePreservesCurrentCLIProfile(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "unexpired"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			now, root := time.Now().UTC(), t.TempDir()
			loadNow := now
			if expired {
				loadNow = now.Add(2 * time.Hour)
			}
			var calls atomic.Int32
			var recovered bootstrap.Material
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body struct {
					RuntimeEnrolled   bool   `json:"runtime_enrolled"`
					Verifier          string `json:"verifier"`
					PublicIdentityKey string `json:"public_identity_key"`
				}
				if r.URL.Path != "/v1/machines/pairings/installation" || json.NewDecoder(r.Body).Decode(&body) != nil ||
					!body.RuntimeEnrolled || body.Verifier != "verifier-012345678901234567890123456789" ||
					body.PublicIdentityKey != base64.RawURLEncoding.EncodeToString(make([]byte, 32)) {
					t.Error("recovery lost exact admitted pairing authority")
					http.Error(w, "invalid recovery", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": recovered})
			}))
			defer server.Close()
			previous := testClientBootstrapMaterial(server.URL, now.Add(time.Hour))
			publicKey := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			record := bootstrap.NewResumeRecord(server.URL, publicKey, "token", "Laptop",
				"verifier-012345678901234567890123456789", now.Add(time.Hour))
			record.Material, record.PairingStarted = &previous, true
			record.ClientInstalled, record.RuntimeEnrolled = true, true
			if err := bootstrap.SaveResume(root, record); err != nil {
				t.Fatal(err)
			}
			loaded, loadErr := bootstrap.LoadResume(root, server.URL, publicKey, "", "Laptop", loadNow)
			recovered = previous
			recovered.ExpiresAt, recovered.ReuseIdentity = loadNow.Add(time.Hour), true
			recovered.EnrollmentID, recovered.EnrollmentCredential, recovered.ClientSession = "", "", nil
			profiles := testBootstrapProfileStore(t)
			credential := testBootstrapCredential("current-after-normal-refresh")
			if err := profiles.Save(config.Profile{Issuer: server.URL, Account: config.Account{ID: "account_1"},
				CLIClientSessionID: previous.ClientSession.SessionID, AccessExpiresAt: credential.ExpiresAt}, credential); err != nil {
				t.Fatal(err)
			}
			input := testOneShotResumeInput(root, server.URL, publicKey, "", loaded, loadErr)
			input.Config.HTTP = server.Client()
			operations := testOneShotResumeOperations(loadNow)
			operations.RecoverMaterial = bootstrap.RecoverMaterial
			material, journal, err := resumeOneShotEnrollment(context.Background(), input, operations)
			if err != nil {
				t.Fatalf("admitted recovery: %v", err)
			}
			if calls.Load() != 1 || material.ClientSession != nil || !journal.ClientInstalled || !journal.RuntimeEnrolled {
				t.Fatal("recovery reused cached CLI credentials or lost installed progress")
			}
			if err := completeBootstrapCLIResume(context.Background(), root, server.URL, material, &journal,
				func(context.Context, *bootstrap.ClientSession, string) error {
					t.Fatal("admitted recovery attempted a fresh CLI login")
					return nil
				}, bootstrap.SaveResume); err != nil {
				t.Fatal(err)
			}
			actual, err := profiles.CredentialFor(server.URL)
			if err != nil || actual.AccessToken != credential.AccessToken || actual.RefreshToken != credential.RefreshToken {
				t.Fatal("recovery replaced current normal-refresh credentials")
			}
			reloaded, err := bootstrap.LoadResume(root, server.URL, publicKey, "", "Laptop", loadNow)
			if err != nil || reloaded.Material.ClientSession != nil || !reloaded.ClientInstalled || !reloaded.RuntimeEnrolled {
				t.Fatal("admitted identity-only recovery was not durably checkpointed")
			}
		})
	}
}
