package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// The real HTTP/SQL boundary owns atomic retirement and lost-response recovery.
// It must never require the old credentials that retirement correctly revokes.
func TestPairingResetAtomicAccountBindingAndRecoveryPostgres(t *testing.T) {
	f := newPairingResetFixture(t)
	checkPairingResetDenials(t, f)
	checkPairingResetRecovery(t, f)
}

type pairingResetFixture struct {
	ctx           context.Context
	store         *db.DB
	service       *usermachines.Service
	suffix, owner string
	refs          []usermachines.ResetMachine
	exec          func(string, ...any)
	post          func(usermachines.PairingResetInput) *httptest.ResponseRecorder
	snapshot      func() string
}

func newPairingResetFixture(t *testing.T) pairingResetFixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated VPS PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, foreign := "reset-owner-"+suffix, "reset-foreign-"+suffix
	for _, account := range []string{owner, foreign} {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	}
	refs := make([]usermachines.ResetMachine, 3)
	privateKeys := map[string]ed25519.PrivateKey{}
	for i := range refs {
		account := owner
		if i == 2 {
			account = foreign
		}
		pub, private, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		m := usermachines.ResetMachine{ID: fmt.Sprintf("reset-machine-%d-%s", i, suffix), PublicIdentityKey: base64.RawURLEncoding.EncodeToString(pub), InstallationGeneration: 1}
		refs[i] = m
		privateKeys[m.PublicIdentityKey] = private
		exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, m.ID, account)
		exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation) VALUES($1,$2,$1,$3,'linux','amd64','/workspace','online','occupied',$4,1)`, m.ID, account, fmt.Sprintf("host-%d", i%2), m.PublicIdentityKey)
		exec(`INSERT INTO cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,user_machine_id) VALUES($1,$2,'paperboat','reset test','desktop','linux',ARRAY['machines:connect'],'active',now(),now(),$3)`, m.ID+"-cli", account, m.ID)
		exec(`INSERT INTO cli_access_tokens(token_hash,cli_client_session_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, m.ID+"-access", m.ID+"-cli")
		exec(`INSERT INTO cli_refresh_tokens(token_hash,cli_client_session_id,state,expires_at,created_at) VALUES($1,$2,'active',now()+interval '1 hour',now())`, m.ID+"-refresh", m.ID+"-cli")
	}
	service := usermachines.New(store, audit.NewWriter(store), usermachines.Policy{PairingLifetime: time.Minute, AllowedPlatforms: []string{"linux"}}, nil)
	service.ConfigureInstallationEncryption("reset-test-encryption")
	router := NewRouter(Options{Auth: &auth.Service{}, Machines: service, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	post := func(in usermachines.PairingResetInput) *httptest.ResponseRecorder {
		t.Helper()
		in.Machines = append([]usermachines.ResetMachine(nil), in.Machines...)
		for i, m := range in.Machines {
			if m.IdentitySignature != "" {
				continue
			}
			token := sha256.Sum256([]byte(in.EnrollmentToken))
			verifier := sha256.Sum256([]byte(in.Verifier))
			payload := []byte(strings.Join([]string{"paperboat.machine-reset/v1", hex.EncodeToString(token[:]), hex.EncodeToString(verifier[:]), in.Platform, in.Architecture, m.ID, m.PublicIdentityKey, strconv.FormatInt(m.InstallationGeneration, 10), ""}, "\n"))
			in.Machines[i].IdentitySignature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKeys[m.PublicIdentityKey], payload))
		}
		body, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/machines/pairings/reset", bytes.NewReader(body))
		router.ServeHTTP(w, r)
		if bytes.Contains(w.Body.Bytes(), []byte(in.EnrollmentToken)) || bytes.Contains(w.Body.Bytes(), []byte(in.Verifier)) {
			t.Fatal("reset response disclosed a recovery secret")
		}
		return w
	}
	snapshot := func() string {
		t.Helper()
		var value string
		if err := store.SQL().QueryRowContext(ctx, `SELECT jsonb_build_object(
'enrollments',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM user_machine_pairings p WHERE bootstrap_owner_account=ANY($1::text[])),
'resets',(SELECT jsonb_agg(to_jsonb(r) ORDER BY enrollment_id,enrollment_generation) FROM user_machine_pairing_resets r WHERE account_id=ANY($1::text[])),
'machines',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM user_machines m WHERE user_id=ANY($1::text[])),
'environments',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM control_environments e WHERE owner_user_id=ANY($1::text[])),
'sessions',(SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM cli_client_sessions s WHERE user_id=ANY($1::text[])),
'access',(SELECT jsonb_agg(to_jsonb(a) ORDER BY token_hash) FROM cli_access_tokens a JOIN cli_client_sessions s ON s.id=a.cli_client_session_id WHERE s.user_id=ANY($1::text[])),
'refresh',(SELECT jsonb_agg(to_jsonb(r) ORDER BY token_hash) FROM cli_refresh_tokens r JOIN cli_client_sessions s ON s.id=r.cli_client_session_id WHERE s.user_id=ANY($1::text[])))::text`, []string{owner, foreign}).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	return pairingResetFixture{ctx, store, service, suffix, owner, refs, exec, post, snapshot}
}

func checkPairingResetDenials(t *testing.T, f pairingResetFixture) {
	t.Helper()
	ctx, service, owner, suffix, refs, exec, post, snapshot := f.ctx, f.service, f.owner, f.suffix, f.refs, f.exec, f.post, f.snapshot
	for i, name := range []string{"foreign account", "wrong key", "wrong generation", "missing machine", "duplicate", "forged identity proof", "malformed identity proof", "wrong platform", "expired", "revoked"} {
		t.Run(name, func(t *testing.T) {
			enrollment, err := service.StartEnrollment(ctx, owner, fmt.Sprintf("reset-negative-%s-%d", suffix, i))
			if err != nil {
				t.Fatal(err)
			}
			in := usermachines.PairingResetInput{EnrollmentToken: enrollment.BootstrapToken, Verifier: "saved-reset-verifier-" + suffix, Platform: "linux", Architecture: "amd64", Machines: append([]usermachines.ResetMachine(nil), refs[:2]...)}
			want, code := 403, "pairing_reset_denied"
			switch name {
			case "foreign account":
				in.Machines[1] = refs[2]
			case "wrong key":
				in.Machines[1].PublicIdentityKey = refs[2].PublicIdentityKey
			case "wrong generation":
				in.Machines[1].InstallationGeneration++
			case "missing machine":
				in.Machines[1].ID = "missing-" + suffix
			case "duplicate":
				in.Machines[1] = in.Machines[0]
				want, code = 400, "pairing_reset_invalid"
			case "forged identity proof":
				in.Machines[1].IdentitySignature = base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
			case "malformed identity proof":
				in.Machines[1].IdentitySignature = "not-a-signature"
				want, code = 400, "pairing_reset_invalid"
			case "wrong platform":
				in.EnrollmentToken = "1" + in.EnrollmentToken[1:]
				want, code = 400, "pairing_platform_unsupported"
			case "expired":
				exec(`UPDATE user_machine_pairings SET expires_at=now()-interval '1 second' WHERE id=$1`, enrollment.ID)
				want, code = 410, "enrollment_token_expired"
			case "revoked":
				exec(`UPDATE user_machine_pairings SET state='expired',installation_state='revoked' WHERE id=$1`, enrollment.ID)
				want, code = 400, "enrollment_token_invalid"
			}
			before := snapshot()
			w := post(in)
			var out ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("reset returned non-JSON status%d", w.Code)
			}
			if w.Code != want || out.Error.Code != code {
				t.Fatalf("reset %s status%d code%s want%d/%s", name, w.Code, out.Error.Code, want, code)
			}
			if snapshot() != before {
				t.Fatal("rejected batch partially retired a machine or credential")
			}
		})
	}
}

func checkPairingResetRecovery(t *testing.T, f pairingResetFixture) {
	t.Helper()
	ctx, store, service, owner, suffix, refs, exec, post, snapshot := f.ctx, f.store, f.service, f.owner, f.suffix, f.refs, f.exec, f.post, f.snapshot
	enrollment, err := service.StartEnrollment(ctx, owner, "reset-success-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	in := usermachines.PairingResetInput{EnrollmentToken: enrollment.BootstrapToken, Verifier: "saved-reset-verifier-" + suffix, Platform: "linux", Architecture: "amd64", Machines: refs[:2]}
	var responses [2]*httptest.ResponseRecorder
	var group sync.WaitGroup
	for i := range responses {
		group.Add(1)
		go func() { defer group.Done(); responses[i] = post(in) }()
	}
	group.Wait()
	for _, response := range responses {
		if response == nil || response.Code != 200 {
			t.Fatal("concurrent exact reset did not resolve to one committed receipt")
		}
	}
	w := responses[0]
	if !bytes.Equal(responses[0].Body.Bytes(), responses[1].Body.Bytes()) {
		t.Fatal("concurrent reset returned different receipts")
	}
	if w.Code != 200 {
		t.Fatalf("reset status%d", w.Code)
	}
	var first struct {
		Data usermachines.PairingReset `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Data.State != "retired" || first.Data.EnrollmentID != enrollment.ID || !reflect.DeepEqual(first.Data.RetiredMachineIDs, []string{refs[0].ID, refs[1].ID}) {
		t.Fatal("reset receipt lost exact account-bound resources")
	}
	for _, m := range refs[:2] {
		var retired bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT m.state='deleted' AND m.seat_state='released' AND e.desired_state='revoked' AND s.state='revoked' AND a.revoked_at IS NOT NULL AND r.state='revoked' FROM user_machines m JOIN control_environments e ON e.id=m.environment_id JOIN cli_client_sessions s ON s.user_machine_id=m.id JOIN cli_access_tokens a ON a.cli_client_session_id=s.id JOIN cli_refresh_tokens r ON r.cli_client_session_id=s.id WHERE m.id=$1`, m.ID).Scan(&retired); err != nil {
			t.Fatal(err)
		}
		if !retired {
			t.Fatal("old machine or its credentials remained usable")
		}
	}
	before := snapshot()
	in.Machines = []usermachines.ResetMachine{refs[1], refs[0]}
	// Discard the first response, then retry without any old account credential.
	retry := post(in)
	var recovered struct {
		Data usermachines.PairingReset `json:"data"`
	}
	if err := json.Unmarshal(retry.Body.Bytes(), &recovered); err != nil {
		t.Fatal(err)
	}
	if retry.Code != 200 || !reflect.DeepEqual(first, recovered) || snapshot() != before {
		t.Fatal("lost-response retry did not return immutable retirement receipt")
	}
	in.Verifier = "different-reset-verifier-" + suffix
	if w := post(in); w.Code != 409 {
		t.Fatalf("different verifier accepted reset receipt: %d", w.Code)
	}
	in.Verifier = "saved-reset-verifier-" + suffix
	in.Machines = append(in.Machines, refs[2])
	if w := post(in); w.Code != 409 {
		t.Fatalf("changed reset batch accepted: %d", w.Code)
	}
	// The fresh pairing must use the reset's protected verifier too.
	_, err = service.CreatePairing(ctx, usermachines.PairingInput{EnrollmentToken: enrollment.BootstrapToken, Verifier: "different-reset-verifier-" + suffix, Alias: "replacement", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace", PublicIdentityKey: refs[2].PublicIdentityKey})
	if err != usermachines.ErrPairingResetConflict {
		t.Fatal("pairing escaped reset's verifier binding")
	}
	if snapshot() != before {
		t.Fatal("receipt misuse modified machine, enrollment or recovery state")
	}
	in.Machines = refs[:2]
	exec(`UPDATE user_machine_pairings SET expires_at=now()-interval '1 second' WHERE id=$1`, enrollment.ID)
	before = snapshot()
	if w := post(in); w.Code != 200 {
		t.Fatalf("completed receipt expired before lost-response recovery: %d", w.Code)
	}
	if snapshot() != before {
		t.Fatal("expired receipt recovery modified state")
	}
	exec(`UPDATE user_machine_pairings SET state='expired',installation_state='revoked' WHERE id=$1`, enrollment.ID)
	before = snapshot()
	if w := post(in); w.Code != 400 {
		t.Fatalf("explicit token revocation did not cut receipt recovery: %d", w.Code)
	}
	if snapshot() != before {
		t.Fatal("receipt misuse modified already-retired or foreign resources")
	}
}
