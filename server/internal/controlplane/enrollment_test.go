package controlplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/testutil"
)

func TestEnrollmentExchangeRejectionClassUsesTypedCredentialCode(t *testing.T) {
	for code, want := range map[mint.CredentialVerificationCode]string{
		mint.CredentialMalformed:        "token_malformed",
		mint.CredentialSignatureInvalid: "token_signature",
		mint.CredentialExpired:          "token_expired",
		mint.CredentialClaimsInvalid:    "token_claims",
	} {
		err := errors.Join(errEnrollmentToken, mint.CredentialVerificationError{Code: code})
		if got := EnrollmentExchangeRejectionClass(err); got != want {
			t.Errorf("code %q classified as %q, want %q", code, got, want)
		}
	}
	textOnly := errors.Join(errEnrollmentToken, errors.New("credential signature is invalid"))
	if got := EnrollmentExchangeRejectionClass(textOnly); got != "token_binding" {
		t.Fatalf("message text selected rejection class %q", got)
	}
}

func TestHelperEnrollmentExchangeIsSingleUseAndKeyBound(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	environmentID := "env_enrollment_" + strings.ReplaceAll(t.Name(), "/", "_")
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ('usr_test','workos_enrollment','enrollment@example.test','active') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$2,'usr_test') ON CONFLICT (id) DO UPDATE SET owner_user_id='usr_test'`, environmentID, "workspace_test"); err != nil {
		t.Fatal(err)
	}
	seedEnrollmentMachine(t, store, "usr_test", environmentID)
	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer, err := mint.New([]mint.Key{{ID: "enrollment-test", PrivateKey: privateKey}}, "enrollment-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := NewEnrollmentService(store, signer, nil, "https://api.example.test", "enrollment-test-encryption-key")
	service.clock = func() time.Time { return now }
	operationKey := "issue:" + environmentID
	grant, err := service.Issue(ctx, "usr_test", operationKey, environmentID, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.Issue(ctx, "usr_test", operationKey, environmentID, 5*time.Minute)
	if err != nil || replay != grant {
		t.Fatalf("issuance replay = %#v, %v; want %#v", replay, err, grant)
	}
	if _, err := service.Issue(ctx, "usr_test", operationKey, environmentID, 6*time.Minute); !errors.Is(err, ErrUsageOperationConflict) {
		t.Fatalf("conflicting issuance error = %v", err)
	}
	var ciphertext []byte
	if err := store.SQL().QueryRowContext(ctx, `SELECT grant_ciphertext FROM paperboat.control_helper_enrollments WHERE id=$1`, grant.EnrollmentID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), grant.Credential) {
		t.Fatal("stored enrollment grant contains plaintext credential")
	}
	helperPrivate := enrollmentTestPrivateKey(environmentID)
	helperPublic := helperPrivate.Public().(ed25519.PublicKey)
	identity, err := service.Exchange(ctx, grant.Credential, helperPublic)
	if err != nil {
		t.Fatal(err)
	}
	if identity.EnvironmentID != environmentID || identity.HelperID != grant.HelperID || identity.Credential == "" {
		t.Fatalf("identity = %#v", identity)
	}
	claims, err := signer.VerifyCredential(identity.Credential, "https://api.example.test", "helper_identity", now)
	thumbprint := sha256.Sum256(helperPublic)
	if err != nil || claims.HelperID != grant.HelperID || claims.KeyThumbprint != "sha256:"+base64.RawURLEncoding.EncodeToString(thumbprint[:]) {
		t.Fatalf("identity claims = %#v, %v", claims, err)
	}
	replayed, err := service.Exchange(ctx, grant.Credential, helperPublic)
	if err != nil || replayed.HelperID != identity.HelperID || replayed.MachineID != identity.MachineID || replayed.Credential == identity.Credential {
		t.Fatalf("same-key exchange replay = %#v, %v; first = %#v", replayed, err, identity)
	}
	otherPublic := ed25519.NewKeyFromSeed([]byte(strings.Repeat("z", ed25519.SeedSize))).Public().(ed25519.PublicKey)
	if _, err := service.Exchange(ctx, grant.Credential, otherPublic); !errors.Is(err, ErrEnrollmentUsed) {
		t.Fatalf("different-key replay error = %v", err)
	}
	renewNow := now.Add(30 * 24 * time.Hour)
	service.clock = func() time.Time { return renewNow }
	renewBody := []byte(`{"operation_id":"helper-renew-operation-01"}`)
	renewHash := sha256.Sum256(renewBody)
	renewClaims := HelperProofClaims{HelperID: identity.HelperID, EnvironmentID: environmentID, OperationID: "helper-renew-operation-01", Method: "POST", Path: "/v1/helper-identity-renewals", BodySHA256: base64.RawURLEncoding.EncodeToString(renewHash[:]), IssuedAt: renewNow, ExpiresAt: renewNow.Add(time.Minute)}
	renewPayload, _ := json.Marshal(renewClaims)
	renewProof, _ := json.Marshal(helperProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(renewPayload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(helperPrivate, renewPayload))})
	renewed, err := service.Renew(ctx, renewProof, renewBody)
	if err != nil || renewed.Credential == identity.Credential || !renewed.ExpiresAt.After(identity.ExpiresAt) {
		t.Fatalf("renewed identity = %#v, %v", renewed, err)
	}
	renewedReplay, err := service.Renew(ctx, renewProof, renewBody)
	if err != nil || renewedReplay != renewed {
		t.Fatalf("renewal replay = %#v, %v", renewedReplay, err)
	}
	var renewalCiphertext []byte
	if err := store.SQL().QueryRowContext(ctx, `SELECT identity_ciphertext FROM paperboat.hosted_helper_identity_renewals WHERE helper_id=$1`, identity.HelperID).Scan(&renewalCiphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(renewalCiphertext, []byte(renewed.Credential)) {
		t.Fatal("stored renewal contains plaintext credential")
	}
	replacement, err := service.ReplaceHelper(ctx, "usr_test", "replace-helper-01", environmentID, grant.HelperID, "default")
	if err != nil || replacement.ConnectorGeneration != 2 {
		t.Fatalf("replacement = %#v, %v", replacement, err)
	}
	replacementReplay, err := service.ReplaceHelper(ctx, "usr_test", "replace-helper-01", environmentID, grant.HelperID, "default")
	if err != nil || replacementReplay != replacement {
		t.Fatalf("replacement replay = %#v, %v", replacementReplay, err)
	}
	if _, err := service.ReplaceHelper(ctx, "usr_test", "replace-helper-02", environmentID, grant.HelperID, "default"); !errors.Is(err, ErrUsageOperationConflict) {
		t.Fatalf("replacement conflict = %v", err)
	}
}

func TestMachineControlRequestRejectsSupersededAndRevokedSessions(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	suffix := strings.ReplaceAll(t.Name(), "/", "_")
	userID, machineID, environmentID := "usr_mc_"+suffix, "mch_mc_"+suffix, "env_mc_"+suffix
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,installation_generation) VALUES ($1,$2,$3,$1,'linux','amd64','/workspace','online','occupied',true,$4,3)`, machineID, userID, environmentID, base64.RawURLEncoding.EncodeToString(publicKey)); err != nil {
		t.Fatal(err)
	}
	for generation, values := range []struct{ operation, jti string }{{"machine-control-old-" + suffix, "mcc_old_" + suffix}, {"machine-control-current-" + suffix, "mcc_current_" + suffix}} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.machine_control_renewals (operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation,superseded_at) VALUES ($1,$2,3,$3,$4,$5,$6,$7)`, values.operation, machineID, values.jti, now.Add(time.Duration(generation)*time.Minute), now.Add(time.Hour), generation+1, func() any {
			if generation == 0 {
				return now
			}
			return nil
		}()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.machine_control_sessions (machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) VALUES ($1,3,2,$2,$3,$4,$5)`, machineID, "machine-control-current-"+suffix, "mcc_current_"+suffix, now.Add(time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint := sha256.Sum256(publicKey)
	sign := func(jti string, generation int64) string {
		t.Helper()
		token, err := signer.SignCredential(mint.CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-control", Subject: machineID, JTI: jti, IssuedAt: now, ExpiresAt: now.Add(time.Hour), CredentialClass: "machine_control", Scopes: []string{"machine:connect", "machine:renew"}, EnvironmentID: environmentID, MachineID: machineID, UserID: userID, KeyThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(thumbprint[:]), InstallationGeneration: 3, SessionGeneration: generation})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	body := []byte(`{"route_id":"route_1"}`)
	digest := sha256.Sum256(body)
	proofClaims := canonicalMachineProofClaims{MachineID: machineID, EnvironmentID: environmentID, InstallationGeneration: 3, OperationID: "private-access-operation-1", Method: "POST", Path: GrantPathForMachineProofTest, BodySHA256: base64.RawURLEncoding.EncodeToString(digest[:]), IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	payload, _ := json.Marshal(proofClaims)
	proof, _ := json.Marshal(helperProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))})
	service := NewEnrollmentService(store, signer, nil, "https://api.example.test", "")
	service.clock = func() time.Time { return now }
	if _, err := service.VerifyMachineControlRequest(ctx, sign("mcc_old_"+suffix, 1), proof, "POST", GrantPathForMachineProofTest, body); !errors.Is(err, ErrHelperProof) {
		t.Fatalf("superseded credential err=%v", err)
	}
	claims, err := service.VerifyMachineControlRequest(ctx, sign("mcc_current_"+suffix, 2), proof, "POST", GrantPathForMachineProofTest, body)
	if err != nil || claims.CredentialJTI != "mcc_current_"+suffix || claims.SessionGeneration != 2 {
		t.Fatalf("current claims=%+v err=%v", claims, err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET revoked_at=$2,state='revoked',online=false WHERE id=$1`, machineID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyMachineControlRequest(ctx, sign("mcc_current_"+suffix, 2), proof, "POST", GrantPathForMachineProofTest, body); !errors.Is(err, ErrHelperProof) {
		t.Fatalf("revoked credential err=%v", err)
	}
}

const GrantPathForMachineProofTest = "/v1/edge/private-access/grants"

func TestHelperEnrollmentReissuesExpiredCredentialForSamePendingHelper(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	environmentID := "env_reissue_expired_" + strings.ReplaceAll(t.Name(), "/", "_")
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ('usr_test','workos_reissue','reissue@example.test','active') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$2,'usr_test')`, environmentID, "workspace_reissue"); err != nil {
		t.Fatal(err)
	}
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := NewEnrollmentService(store, signer, nil, "https://api.example.test", "enrollment-test-encryption-key")
	service.clock = func() time.Time { return now }
	first, err := service.Issue(ctx, "usr_test", "reissue-first", environmentID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service.clock = func() time.Time { return now.Add(2 * time.Minute) }
	second, err := service.Issue(ctx, "usr_test", "reissue-second", environmentID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.HelperID != first.HelperID || second.EnrollmentID == first.EnrollmentID || second.Credential == first.Credential {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	var firstState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.control_helper_enrollments WHERE id=$1`, first.EnrollmentID).Scan(&firstState); err != nil || firstState != "revoked" {
		t.Fatalf("first state=%q err=%v", firstState, err)
	}
	var helperCount int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.control_helpers WHERE environment_id=$1`, environmentID).Scan(&helperCount); err != nil || helperCount != 1 {
		t.Fatalf("helper count=%d err=%v", helperCount, err)
	}
}

func TestHelperEnrollmentRejectsExpiredCredentialBeforeMutation(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	environmentID := "env_expired_enrollment"
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ('usr_test','workos_enrollment','enrollment@example.test','active') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,'workspace_test','usr_test') ON CONFLICT (id) DO UPDATE SET owner_user_id='usr_test'`, environmentID); err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("x", ed25519.SeedSize)))
	signer, err := mint.New([]mint.Key{{ID: "enrollment-test", PrivateKey: privateKey}}, "enrollment-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := NewEnrollmentService(store, signer, nil, "https://api.example.test", "enrollment-test-encryption-key")
	service.clock = func() time.Time { return now }
	grant, err := service.Issue(ctx, "usr_test", "issue:"+environmentID, environmentID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service.clock = func() time.Time { return now.Add(2 * time.Minute) }
	helperPublic := ed25519.NewKeyFromSeed([]byte(strings.Repeat("h", ed25519.SeedSize))).Public().(ed25519.PublicKey)
	if _, err := service.Exchange(ctx, grant.Credential, helperPublic); !errors.Is(err, ErrEnrollmentInvalid) {
		t.Fatalf("expired exchange error = %v", err)
	}
}

func TestEnsureBootGrantReplacesExpiredGrantAndStopsAfterEnrollment(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	environmentID := "env_boot_grant_" + strings.ReplaceAll(t.Name(), "/", "_")
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ('usr_test','workos_boot_grant','boot-grant@example.test','active') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$1,'usr_test')`, environmentID); err != nil {
		t.Fatal(err)
	}
	seedEnrollmentMachine(t, store, "usr_test", environmentID)
	signer, err := mint.New([]mint.Key{{ID: "boot-grant-test", PrivateKey: ed25519.NewKeyFromSeed([]byte(strings.Repeat("b", ed25519.SeedSize)))}}, "boot-grant-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := NewEnrollmentService(store, signer, nil, "https://api.example.test", "boot-grant-encryption-key")
	service.clock = func() time.Time { return now }
	first, err := service.EnsureBootGrant(ctx, "usr_test", "hosted-create:"+environmentID, environmentID, time.Minute)
	if err != nil || first.Credential == "" {
		t.Fatalf("first grant = %#v, %v", first, err)
	}
	service.clock = func() time.Time { return now.Add(2 * time.Minute) }
	replacement, err := service.EnsureBootGrant(ctx, "usr_test", "hosted-create:"+environmentID, environmentID, time.Minute)
	if err != nil || replacement.Credential == "" || replacement.EnrollmentID == first.EnrollmentID {
		t.Fatalf("replacement grant = %#v, %v", replacement, err)
	}
	var firstState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.control_helper_enrollments WHERE id=$1`, first.EnrollmentID).Scan(&firstState); err != nil || firstState != "revoked" {
		t.Fatalf("first grant state = %q, %v", firstState, err)
	}
	helperKey := enrollmentTestPrivateKey(environmentID)
	if _, err := service.Exchange(ctx, replacement.Credential, helperKey.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	afterEnrollment, err := service.EnsureBootGrant(ctx, "usr_test", "hosted-create:"+environmentID, environmentID, time.Minute)
	if err != nil || afterEnrollment.Credential != "" {
		t.Fatalf("post-enrollment grant = %#v, %v", afterEnrollment, err)
	}
}

func TestEnsureBootGrantConcurrentCallsReplayOneGrant(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	environmentID := "env_boot_concurrent_" + strings.ReplaceAll(t.Name(), "/", "_")
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ('usr_test','workos_boot_concurrent','boot-concurrent@example.test','active') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$1,'usr_test')`, environmentID); err != nil {
		t.Fatal(err)
	}
	signer, err := mint.New([]mint.Key{{ID: "boot-concurrent-test", PrivateKey: ed25519.NewKeyFromSeed([]byte(strings.Repeat("c", ed25519.SeedSize)))}}, "boot-concurrent-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := NewEnrollmentService(store, signer, nil, "https://api.example.test", "boot-concurrent-encryption-key")
	service.clock = func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) }
	const callers = 8
	grants := make(chan EnrollmentGrant, callers)
	errs := make(chan error, callers)
	var start sync.WaitGroup
	start.Add(1)
	for range callers {
		go func() {
			start.Wait()
			grant, err := service.EnsureBootGrant(ctx, "usr_test", "hosted-create:"+environmentID, environmentID, time.Minute)
			grants <- grant
			errs <- err
		}()
	}
	start.Done()
	var enrollmentID string
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		grant := <-grants
		if enrollmentID == "" {
			enrollmentID = grant.EnrollmentID
		} else if grant.EnrollmentID != enrollmentID {
			t.Fatalf("grant IDs differ: %q and %q", enrollmentID, grant.EnrollmentID)
		}
	}
	var pending int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.control_helper_enrollments WHERE environment_id=$1 AND state='pending' AND revoked_at IS NULL`, environmentID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending enrollments=%d err=%v", pending, err)
	}
}

func TestConnectorAdmissionBindsProofGenerationNodeAndReplay(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	environmentID := "env_admission_" + suffix
	seedUsageScope(t, store, suffix)
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ('usr_test','workos_admission','admission@example.test','active') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_environments SET owner_user_id='usr_test' WHERE id=$1`, "env_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET state='ready',ready=true,endpoint_host='edge.example.test',endpoint_tcp_port=26023,endpoint_quic_port=26023,last_heartbeat_at=$2 WHERE id=$1`, "node_"+suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,'workspace_test','usr_test')`, environmentID); err != nil {
		t.Fatal(err)
	}
	seedEnrollmentMachine(t, store, "usr_test", environmentID)
	privateKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("a", ed25519.SeedSize)))
	signer, err := mint.New([]mint.Key{{ID: "admission-test", PrivateKey: privateKey}}, "admission-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment := NewEnrollmentService(store, signer, nil, "https://api.example.test", "admission-encryption-key")
	enrollment.clock = func() time.Time { return now }
	grant, err := enrollment.Issue(ctx, "usr_test", "admission-enroll-01", environmentID, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	helperPrivate := enrollmentTestPrivateKey(environmentID)
	identity, err := enrollment.Exchange(ctx, grant.Credential, helperPrivate.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET seat_state='occupied' WHERE id=$1`, identity.MachineID); err != nil {
		t.Fatal(err)
	}
	tunnelID := "tunnel_admission_" + suffix
	tunnelRouteID := "tunnel_route_admission_" + suffix
	endpointID := testutil.EndpointID("connector-admission:" + suffix)
	endpoint := "https://" + testutil.EndpointLabel(endpointID) + ".tunnels.example.test"
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.tunnel_routes WHERE tunnel_id=$1`, tunnelID)
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.tunnels WHERE id=$1`, tunnelID)
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnels (id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES ($1,'usr_test','admission',$2,$3,$4,'usr_test')`, tunnelID, endpointID, endpoint, identity.MachineID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_routes (id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES ($1,$2,'admission','http','exact','admission.example.test','/','http','127.0.0.1:8080','usr_test','usr_test')`, tunnelRouteID, tunnelID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,target_host,target_port) VALUES ($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080)`, "route_admission_"+suffix, environmentID, "admission-"+strings.ToLower(strings.ReplaceAll(suffix, "_", "-"))+".example.test"); err != nil {
		t.Fatal(err)
	}
	// Bind the connector generation to the ready node after enrollment.
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_connector_generations SET edge_node_id=$2,state='pending' WHERE environment_id=$1`, environmentID, "node_"+suffix); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"operation_id":"admission-operation-01","environment_id":"` + environmentID + `","machine_id":"` + identity.MachineID + `","connector_id":"runtime","edge_pool":"default","protocol_version":"1.0"}`)
	bodyHash := sha256.Sum256(body)
	proofClaims := HelperProofClaims{HelperID: identity.HelperID, EnvironmentID: environmentID, OperationID: "admission-operation-01", Method: "POST", Path: "/v1/connectors/admission", BodySHA256: base64.RawURLEncoding.EncodeToString(bodyHash[:]), IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	payload, _ := json.Marshal(proofClaims)
	proof, _ := json.Marshal(helperProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(helperPrivate, payload))})
	edge := NewEdgeService(store, "edge-control-credential-01234567890123456789")
	edge.SetClock(func() time.Time { return now })
	edge.SetCredentialIssuer(signer, "https://api.example.test", "admission-encryption-key")
	admission, err := edge.IssueConnectorAdmission(ctx, identity.Credential, proof, body, "default", "POST", "/v1/connectors/admission")
	if err != nil || admission.OperationID != "admission-operation-01" || admission.EdgePool != "default" || admission.ProtocolVersion != "1.0" || admission.EdgeNodeID != "node_"+suffix || admission.EdgeEndpoint.Port != 26023 || len(admission.Routes) != 1 || admission.Routes[0].RouteID != "route_admission_"+suffix {
		t.Fatalf("admission = %#v, %v", admission, err)
	}
	wire, err := json.Marshal(admission)
	if err != nil || bytes.Contains(wire, []byte(`"expires_at"`)) || !bytes.Contains(wire, []byte(`"tcp_port":26023`)) || !bytes.Contains(wire, []byte(`"quic_port":26023`)) {
		t.Fatalf("admission wire = %s, %v", wire, err)
	}
	if _, err := signer.VerifyCredential(admission.Credential, "https://api.example.test", "connector_admission", now); err != nil {
		t.Fatalf("admission credential = %v", err)
	}
	replay, err := edge.IssueConnectorAdmission(ctx, identity.Credential, proof, body, "default", "POST", "/v1/connectors/admission")
	if err != nil || replay.Credential != admission.Credential {
		t.Fatalf("admission replay = %#v, %v", replay, err)
	}
	next := now.Add(time.Minute)
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=$2 WHERE id=$1`, "node_"+suffix, next); err != nil {
		t.Fatal(err)
	}
	edge.SetClock(func() time.Time { return next })
	nextBody := []byte(`{"operation_id":"admission-operation-02","environment_id":"` + environmentID + `","machine_id":"` + identity.MachineID + `","connector_id":"runtime","edge_pool":"default","protocol_version":"1.0"}`)
	nextHash := sha256.Sum256(nextBody)
	nextClaims := HelperProofClaims{HelperID: identity.HelperID, EnvironmentID: environmentID, OperationID: "admission-operation-02", Method: "POST", Path: "/v1/connectors/admission", BodySHA256: base64.RawURLEncoding.EncodeToString(nextHash[:]), IssuedAt: next, ExpiresAt: next.Add(time.Minute)}
	nextPayload, _ := json.Marshal(nextClaims)
	nextProof, _ := json.Marshal(helperProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(nextPayload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(helperPrivate, nextPayload))})
	const concurrentRenewals = 16
	renewals := make(chan ConnectorAdmission, concurrentRenewals)
	renewalErrors := make(chan error, concurrentRenewals)
	var renewalGroup sync.WaitGroup
	for range concurrentRenewals {
		renewalGroup.Add(1)
		go func() {
			defer renewalGroup.Done()
			renewed, err := edge.IssueConnectorAdmission(ctx, identity.Credential, nextProof, nextBody, "default", "POST", "/v1/connectors/admission")
			if err != nil {
				renewalErrors <- err
				return
			}
			renewals <- renewed
		}()
	}
	renewalGroup.Wait()
	close(renewals)
	close(renewalErrors)
	for err := range renewalErrors {
		t.Errorf("concurrent admission replacement: %v", err)
	}
	var renewedCredential string
	for renewed := range renewals {
		if renewed.OperationID != "admission-operation-02" || renewed.Credential == admission.Credential {
			t.Errorf("admission replacement = %#v", renewed)
		}
		if renewedCredential == "" {
			renewedCredential = renewed.Credential
		} else if renewedCredential != renewed.Credential {
			t.Errorf("concurrent admission credentials differ")
		}
	}
	newAdmissionRequest := func(operationID string) ([]byte, []byte) {
		body := []byte(`{"operation_id":"` + operationID + `","environment_id":"` + environmentID + `","machine_id":"` + identity.MachineID + `","connector_id":"runtime","edge_pool":"default","protocol_version":"1.0"}`)
		bodyHash := sha256.Sum256(body)
		claims := HelperProofClaims{HelperID: identity.HelperID, EnvironmentID: environmentID, OperationID: operationID, Method: "POST", Path: "/v1/connectors/admission", BodySHA256: base64.RawURLEncoding.EncodeToString(bodyHash[:]), IssuedAt: next, ExpiresAt: next.Add(time.Minute)}
		payload, _ := json.Marshal(claims)
		envelope, _ := json.Marshal(helperProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(helperPrivate, payload))})
		return body, envelope
	}

	routeBody, routeProof := newAdmissionRequest("admission-route-outage-" + suffix)
	if _, err := store.SQL().ExecContext(ctx, `ALTER TABLE paperboat.control_routes RENAME TO control_routes_diagnostics_saved`); err != nil {
		t.Fatal(err)
	}
	routeRequest := httptest.NewRequest(http.MethodPost, "/v1/connectors/admission", bytes.NewReader(routeBody))
	routeRequest.Header.Set("Authorization", "Bearer "+identity.Credential)
	routeRequest.Header.Set("Content-Type", "application/json")
	routeRequest.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(routeProof))
	routeResponse := httptest.NewRecorder()
	edge.Handler().ServeHTTP(routeResponse, routeRequest)
	if _, err := store.SQL().ExecContext(ctx, `ALTER TABLE paperboat.control_routes_diagnostics_saved RENAME TO control_routes`); err != nil {
		t.Fatal(err)
	}
	if routeResponse.Code != http.StatusServiceUnavailable || !strings.Contains(routeResponse.Body.String(), `"code":"control_unavailable"`) || strings.Contains(routeResponse.Body.String(), "does not exist") {
		t.Fatalf("route query outage response = %d %s", routeResponse.Code, routeResponse.Body.String())
	}

	generationBody, generationProof := newAdmissionRequest("admission-generation-outage-" + suffix)
	if _, err := store.SQL().ExecContext(ctx, `ALTER TABLE paperboat.control_connector_generations RENAME TO control_connector_generations_diagnostics_saved`); err != nil {
		t.Fatal(err)
	}
	_, generationErr := edge.IssueConnectorAdmission(ctx, identity.Credential, generationProof, generationBody, "default", "POST", "/v1/connectors/admission")
	if _, err := store.SQL().ExecContext(ctx, `ALTER TABLE paperboat.control_connector_generations_diagnostics_saved RENAME TO control_connector_generations`); err != nil {
		t.Fatal(err)
	}
	if generationErr == nil || errors.Is(generationErr, ErrHelperProof) {
		t.Fatalf("generation query outage lost its operational cause: %v", generationErr)
	}

	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET state='offline', ready=false WHERE id=$1`, "node_"+suffix); err != nil {
		t.Fatal(err)
	}
	unavailableBody, unavailableProof := newAdmissionRequest("admission-no-ready-node-" + suffix)
	_, unavailableErr := edge.IssueConnectorAdmission(ctx, identity.Credential, unavailableProof, unavailableBody, "default", "POST", "/v1/connectors/admission")
	if !errors.Is(unavailableErr, ErrEdgeNodeUnavailable) || errors.Is(unavailableErr, ErrHelperProof) {
		t.Fatalf("no-ready-node outcome = %v, want ErrEdgeNodeUnavailable", unavailableErr)
	}
}

func seedEnrollmentMachine(t *testing.T, store interface{ SQL() *sql.DB }, userID, environmentID string) {
	t.Helper()
	if _, err := store.SQL().ExecContext(context.Background(), `
		INSERT INTO paperboat.user_machines
		  (id,user_id,environment_id,description,platform,architecture,workspace_root)
		VALUES ($1,$2,$3,$4,'linux','unknown','/workspace')
		ON CONFLICT (environment_id) DO NOTHING`,
		"machine_"+environmentID, userID, environmentID, "Machine "+environmentID); err != nil {
		t.Fatal(err)
	}
}

func enrollmentTestPrivateKey(scope string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("enrollment-test:" + scope))
	return ed25519.NewKeyFromSeed(seed[:])
}
