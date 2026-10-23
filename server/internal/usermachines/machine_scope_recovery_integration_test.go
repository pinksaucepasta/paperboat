package usermachines

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

func TestAuthenticatedMachineScopeRefreshPreservesInstallation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	user, client := "user_"+uuid.NewString(), "session_"+uuid.NewString()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, user, user+"@example.test")
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,'paperboat-cli','Scope refresh','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, client, user)
	exec(`INSERT INTO paperboat.user_machine_entitlements(id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES($1,$2,$1,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "period_"+uuid.NewString(), user)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := base64.RawURLEncoding.EncodeToString(public)
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	writer := audit.NewWriter(store)
	enrollments := controlplane.NewEnrollmentService(store, signer, writer, "https://api.paperboat.test", "scope-enrollment-key")
	service := New(store, writer, Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("scope-installation-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureCLIAuth("paperboat-cli", []string{"account:read", "machines:connect", "session:refresh"}, 5*time.Minute, 24*time.Hour, "scope-cli-hash")
	convert := func(g controlplane.EnrollmentGrant) HelperEnrollmentGrant {
		return HelperEnrollmentGrant{EnrollmentID: g.EnrollmentID, HelperID: g.HelperID, Credential: g.Credential, ExpiresAt: g.ExpiresAt}
	}
	service.ConfigureHelperEnrollment(func(ctx context.Context, actor, operation, scope string, lifetime time.Duration) (HelperEnrollmentGrant, error) {
		g, err := enrollments.Issue(ctx, actor, operation, scope, lifetime)
		return convert(g), err
	})
	service.ConfigureHelperRecovery(func(ctx context.Context, actor, operation, scope, helper string, lifetime time.Duration) (HelperEnrollmentGrant, error) {
		g, err := enrollments.RecoverHelper(ctx, actor, operation, scope, helper, lifetime)
		return convert(g), err
	})
	service.ConfigureAuthenticatedHelperEnrollment(
		func(context.Context, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			return HelperEnrollmentGrant{}, errors.New("refresh must replace the installed runtime incarnation")
		},
		func(ctx context.Context, actor, operation, scope, helper string, lifetime time.Duration, guard HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			return PersistAuthenticatedHelperEnrollment(ctx, store, scope, guard, func(ctx context.Context) (HelperEnrollmentGrant, error) {
				g, err := enrollments.RecoverHelper(ctx, actor, operation, scope, helper, lifetime)
				return convert(g), err
			})
		},
	)
	if err = service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	machine, err := service.Setup(ctx, user, SetupInput{Alias: "scope-refresh", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace", PublicIdentityKey: publicKey, RuntimeVersions: json.RawMessage(`{"pb":"test"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if machine.EnvironmentID != machine.ID {
		t.Fatal("setup did not persist the canonical machine control scope")
	}
	initialGrant, err := enrollments.Issue(ctx, user, "initial-scope-runtime", machine.ID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	initialIdentity, err := enrollments.Exchange(ctx, initialGrant.Credential, public)
	if err != nil {
		t.Fatal(err)
	}
	input := AuthenticatedMachineInstallationInput{OperationID: "operation_" + uuid.NewString(), Verifier: "scope-verifier-" + uuid.NewString(), PublicIdentityKey: publicKey, InstallationGeneration: machine.InstallationGeneration, CanReuseRuntimeIdentity: false}
	first, err := service.PrepareAuthenticatedMachineInstallation(ctx, user, client, machine.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.PrepareAuthenticatedMachineInstallation(ctx, user, client, machine.ID, input)
	if err != nil || replay.ExpiresAt != first.ExpiresAt {
		t.Fatalf("same-key preparation replay: %v", err)
	}
	material, err := service.ConsumeInstallationForIdentity(ctx, input.Verifier, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := authenticatedMachineInstallationMaterialBindingFromPlaintext(material)
	if err != nil {
		t.Fatal(err)
	}
	if binding.MachineID != machine.ID || binding.EnvironmentID != machine.ID || binding.Generation != machine.InstallationGeneration || binding.HelperID == initialIdentity.HelperID {
		t.Fatal("refresh changed the installation binding or retained the replaced incarnation")
	}
	var fields struct {
		Credential string `json:"enrollment_credential"`
	}
	if err = json.Unmarshal(material, &fields); err != nil {
		t.Fatal(err)
	}
	refreshed, err := enrollments.Exchange(ctx, fields.Credential, public)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := signer.VerifyCredential(refreshed.Credential, "https://api.paperboat.test", "helper_identity", time.Now().UTC())
	thumbprint := sha256.Sum256(public)
	if err != nil || claims.MachineID != machine.ID || claims.EnvironmentID != machine.ID || claims.KeyThumbprint != "sha256:"+base64.RawURLEncoding.EncodeToString(thumbprint[:]) {
		t.Fatalf("refreshed credential binding: %v", err)
	}
	var intact bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT m.environment_id=m.id AND m.public_identity_key=$2 AND m.installation_generation=$3 AND old.state='replaced' AND fresh.state='active' FROM paperboat.user_machines m JOIN paperboat.control_helpers old ON old.id=$4 JOIN paperboat.control_helpers fresh ON fresh.id=$5 WHERE m.id=$1`, machine.ID, publicKey, machine.InstallationGeneration, initialIdentity.HelperID, refreshed.HelperID).Scan(&intact); err != nil || !intact {
		t.Fatalf("same-key incarnation recovery lost installation ownership: %v %v", intact, err)
	}
}
