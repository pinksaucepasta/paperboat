package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

// This exercises the public rejection contract against actual locked enrollment
// rows, including the guarantee that rejection cannot claim or alter a journal.
func TestPairingTokenErrorsPreserveEnrollmentPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	owner := fmt.Sprintf("pairing-errors-%d", time.Now().UnixNano())
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, owner); err != nil {
		t.Fatal(err)
	}
	service := usermachines.New(store, audit.NewWriter(store), usermachines.Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, nil)
	service.ConfigureInstallationEncryption("test-enrollment-key")
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name, state, code string
		status            int
	}{
		{"expired", "expired", "enrollment_token_expired", http.StatusGone},
		{"claimed", "awaiting_approval", "enrollment_token_used", http.StatusConflict},
		{"claimed past expiry", "awaiting_approval", "enrollment_token_used", http.StatusConflict},
		{"cancelled", "cancelled", "enrollment_token_invalid", http.StatusBadRequest},
		{"revoked", "revoked", "enrollment_token_invalid", http.StatusBadRequest},
		{"unknown", "", "enrollment_token_invalid", http.StatusBadRequest},
		{"malformed", "", "enrollment_token_invalid", http.StatusBadRequest},
		{"unsupported platform", "", "pairing_platform_unsupported", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enrollment, err := service.StartEnrollment(ctx, owner, fmt.Sprintf("%s-%d", owner, i))
			if err != nil {
				t.Fatal(err)
			}
			if tc.state == "awaiting_approval" {
				hash := sha256.Sum256([]byte(enrollment.ID))
				_, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET state='pending',installation_state=$2,verifier_hash=$5,user_code=$3,requested_alias='retained',platform='linux',architecture='amd64',workspace_root='/workspace',public_identity_key=$4 WHERE id=$1`, enrollment.ID, tc.state, fmt.Sprintf("%d%02d", time.Now().UnixNano(), i), base64.RawURLEncoding.EncodeToString(publicKey), hash[:])
			} else if tc.state != "" {
				_, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET state='expired',installation_state=$2 WHERE id=$1`, enrollment.ID, tc.state)
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "expired" || tc.name == "claimed past expiry" {
				if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET expires_at=now()-interval '1 minute' WHERE id=$1`, enrollment.ID); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := func() string {
				t.Helper()
				var value string
				if err := store.SQL().QueryRowContext(ctx, `SELECT row_to_json(p)::text FROM paperboat.user_machine_pairings p WHERE id=$1`, enrollment.ID).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := snapshot()
			token, platform := enrollment.BootstrapToken, "linux"
			switch tc.name {
			case "unknown":
				token = "0ZZZZZZZZZZZZZZZZZZZZZZZZZ"
			case "malformed":
				token = "invalid-token"
			case "unsupported platform":
				platform = "unsupported"
			}
			body, err := json.Marshal(map[string]any{"enrollment_token": token, "verifier": "fresh-unclaimed-verifier", "alias": "fresh-host", "platform": platform, "architecture": "amd64", "workspace_root": "/workspace", "public_identity_key": base64.RawURLEncoding.EncodeToString(publicKey)})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			userMachinePairings(service).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/machines/pairings", bytes.NewReader(body)))
			var result ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal("invalid error envelope")
			}
			if response.Code != tc.status || result.Error.Code != tc.code {
				t.Fatalf("status=%d code=%q, want %d %q", response.Code, result.Error.Code, tc.status, tc.code)
			}
			if bytes.Contains(response.Body.Bytes(), []byte(token)) {
				t.Fatal("error disclosed enrollment token")
			}
			if before != snapshot() {
				t.Fatal("rejection altered enrollment or protected journal")
			}
			var machines int
			if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machines WHERE user_id=$1`, owner).Scan(&machines); err != nil {
				t.Fatal(err)
			}
			if machines != 0 {
				t.Fatal("rejection allocated a machine")
			}
		})
	}
}
