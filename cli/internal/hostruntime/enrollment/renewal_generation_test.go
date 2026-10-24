package enrollment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

// stg-m1: pb install over a running machine moved the installation generation; the carrier kept the
// stored credential from the old generation and the server refused it with 401 for 35 min, until the
// scheduled renewal. A moved generation renews at once.
func TestRenewingTokenSourceRenewsWhenTheInstallationGenerationMoves(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var renewals atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/helper-enrollments":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": "initial-helper-credential-012345678901234567890", "expires_at": now.Add(time.Hour)}})
		case "/v1/helper-identity-renewals":
			renewals.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": "renewed-helper-credential-012345678901234567890", "expires_at": now.Add(2 * time.Hour)}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	if err := makeEnrollmentStateRootPrivateForTest(root); err != nil {
		t.Fatal("protect enrollment test state root")
	}
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	register := func(generation int64) {
		t.Helper()
		if err := store.SaveRegistration(identity.Registration{ServerURL: server.URL, MachineID: "machine_1", EnvironmentID: "env_1", PublicKeyID: store.Current().ID, PublicIdentityKey: "public", InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: generation, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	register(1)
	client, _ := NewClient(server.Client().Transport, time.Second)
	if _, err := client.Enroll(context.Background(), Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: "grant-credential-012345678901234567890"}); err != nil {
		t.Fatal(err)
	}
	source, err := NewRenewingTokenSource(RenewingTokenConfig{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, Timeout: time.Second, Clock: func() time.Time { return now }, OperationID: func() (string, error) { return "op_renew_0001", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if token, err := source.Token(context.Background()); err != nil || token != "initial-helper-credential-012345678901234567890" || renewals.Load() != 0 {
		t.Fatalf("same generation: token=%q err=%v renewals=%d", token, err, renewals.Load())
	}
	register(2)
	for range 2 {
		if token, err := source.Token(context.Background()); err != nil || token != "renewed-helper-credential-012345678901234567890" || renewals.Load() != 1 {
			t.Fatalf("moved generation: token=%q err=%v renewals=%d", token, err, renewals.Load())
		}
	}
}
