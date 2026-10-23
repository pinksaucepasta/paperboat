package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat/server/internal/previewv1"
)

type machinePreviewReadinessFixture struct {
	database                                 *db.DB
	account, machine, preview, suffix, token string
	private                                  ed25519.PrivateKey
	router                                   http.Handler
}

func newMachinePreviewReadinessFixture(t *testing.T) machinePreviewReadinessFixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	account, machine, preview := "read_account_"+suffix, "read_machine_"+suffix, "read_preview_"+suffix
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.preview_leases WHERE id=$1`, preview)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	for _, entry := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, []any{account}},
		{`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root) VALUES($1,$2,$1,'linux','amd64','/workspace')`, []any{machine, account}},
		{`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline) VALUES($1,$4,$2,$2,$3,'session_read','foreground','http','127.0.0.1:3000','public',now()+interval '1 hour')`, []any{preview, account, machine, "https://readiness-" + suffix + ".preview.example.test"}},
	} {
		if _, err := database.SQL().ExecContext(ctx, entry.q, entry.args...); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := previewtunnelstore.New(database)
	if err != nil {
		t.Fatal(err)
	}
	service, err := previewv1.NewService(repository, previewv1.Config{EndpointDomain: "preview.example.test", CursorKey: bytes.Repeat([]byte("r"), 32)})
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(private) })
	now := time.Now().UTC()
	jti := "identity_" + suffix
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET public_identity_key=$2,seat_state='occupied' WHERE id=$1`, machine, base64.RawURLEncoding.EncodeToString(public)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) VALUES($1,$2,1,$1,$3,$4,1)`, jti, machine, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) SELECT machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at FROM paperboat.machine_control_renewals WHERE operation_id=$1`, jti); err != nil {
		t.Fatal(err)
	}
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint := sha256.Sum256(public)
	token, err := signer.SignCredential(mint.CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-control", Subject: machine, JTI: jti, IssuedAt: now, ExpiresAt: now.Add(time.Hour), CredentialClass: "machine_control", Scopes: []string{"machine:connect", "machine:renew"}, EnvironmentID: machine, MachineID: machine, UserID: account, KeyThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(thumbprint[:]), InstallationGeneration: 1, SessionGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	identities := controlplane.NewEnrollmentService(database, signer, nil, "https://api.example.test", "")
	router := NewRouter(Options{PreviewLeases: service, RuntimeIdentity: identities})
	return machinePreviewReadinessFixture{database, account, machine, preview, suffix, token, private, router}
}

func TestMachinePreviewReadinessGetPostgres(t *testing.T) {
	f := newMachinePreviewReadinessFixture(t)
	database, router, private := f.database, f.router, f.private
	account, machine, preview, suffix, token := f.account, f.machine, f.preview, f.suffix, f.token
	ctx := context.Background()
	path := "/v1/previews/" + preview + "/readiness"
	read := func(signed bool, want int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if signed {
			digest := sha256.Sum256(nil)
			proofNow := time.Now().UTC()
			payload, _ := json.Marshal(map[string]any{"machine_id": machine, "environment_id": machine, "installation_generation": 1, "operation_id": "readiness_" + suffix, "method": http.MethodGet, "path": path, "body_sha256": base64.RawURLEncoding.EncodeToString(digest[:]), "issued_at": proofNow, "expires_at": proofNow.Add(time.Minute)})
			proof, _ := json.Marshal(map[string]string{"alg": "EdDSA", "payload": base64.RawURLEncoding.EncodeToString(payload), "signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))})
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("X-Paperboat-Machine-Identity", token)
			request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("readiness GET status=%d want=%d", response.Code, want)
		}
		return response
	}
	read(false, 401)
	for _, terminal := range []string{"active", "stopped", "expired", "owner_lost"} {
		if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET terminal_state=$2,stopped_at=CASE WHEN $2='active' THEN NULL ELSE now() END WHERE id=$1`, preview, terminal); err != nil {
			t.Fatal(err)
		}
		response := read(true, 200)
		var body struct {
			Data previewv1.Preview `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		expected := map[string]string{"active": "allocating", "stopped": "stopped", "expired": "expired", "owner_lost": "owner_disconnected"}[terminal]
		if body.Data.ID != preview || body.Data.State != expected || response.Header().Get("ETag") != previewtunnelapi.ETag(previewv1.Kind, preview, 1) || response.Header().Get("Cache-Control") != "no-store, no-transform" {
			t.Fatal("readiness did not return canonical state/ETag")
		}
	}
	foreign := preview + "_other"
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline) VALUES($1,$3,$2,$2,'foreign_machine','session_foreign','foreground','http','127.0.0.1:3000','public',now()+interval '1 hour')`, foreign, account, "https://foreign-"+suffix+".preview.example.test"); err != nil {
		t.Fatal(err)
	}
	defer database.SQL().ExecContext(ctx, `DELETE FROM paperboat.preview_leases WHERE id=$1`, foreign)
	path = "/v1/previews/" + foreign + "/readiness"
	read(true, 403)
	path = "/v1/previews/" + preview + "/readiness"
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET revoked_at=now() WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	read(true, 401)
}
