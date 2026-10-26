package usermachines

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/access"
	"github.com/pinksaucepasta/paperboat/server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	authservice "github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

type testSeatAuthorizer struct{}

func newIntegrationPublicIdentityKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(public)
}

func (testSeatAuthorizer) ReserveUserMachineSeat(context.Context, *db.Tx, string) error {
	return nil
}

func TestRenameMachineUsesOnlyActiveNames(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_machine_rename_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_rename_"+suffix, "machine-rename-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	for _, machine := range []struct{ id, environmentID, name string }{
		{"mch_rename_active_" + suffix, "env_rename_active_" + suffix, "active"},
		{"mch_rename_other_" + suffix, "env_rename_other_" + suffix, "other"},
		{"mch_rename_deleted_" + suffix, "env_rename_deleted_" + suffix, "reusable"},
	} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace','offline','released')`, machine.id, userID, machine.environmentID, machine.name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET state='deleted',deleted_at=now() WHERE id=$1`, "mch_rename_deleted_"+suffix); err != nil {
		t.Fatal(err)
	}

	service := New(store, audit.NewWriter(store), Policy{}, testSeatAuthorizer{})
	renamed, err := service.Rename(ctx, userID, "mch_rename_other_"+suffix, "  reusable  ")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Alias != "reusable" {
		t.Fatalf("display name = %q, want Reusable", renamed.Alias)
	}
	if _, err := service.Rename(ctx, userID, renamed.ID, "active"); !errors.Is(err, ErrMachineNameConflict) {
		t.Fatalf("active-name rename error = %v, want ErrMachineNameConflict", err)
	}
	if _, err := service.Rename(ctx, userID, renamed.ID, " "); !errors.Is(err, ErrInvalidMachineName) {
		t.Fatalf("blank-name rename error = %v, want ErrInvalidMachineName", err)
	}
}

func TestSetupIsIdempotentAndPreservesCapabilities(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_machine_setup_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "machine-setup-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_setup_"+suffix, userID, "sub_setup_"+suffix); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	input := SetupInput{Alias: "studio", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/paperboat", PublicIdentityKey: base64.RawURLEncoding.EncodeToString(public), RuntimeVersions: json.RawMessage(`{"pb":"test"}`)}
	service := New(store, audit.NewWriter(store), Policy{AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	first, err := service.Setup(ctx, userID, input)
	if err != nil {
		t.Fatal(err)
	}
	// A newly installed machine has its local-address lease at once: machine lists only read leases, and
	// without one its daemon cannot bind the machine's local address (prod 2026-10-25: private access failed
	// with EINVAL on 14 new machines).
	var leaseSlot int
	if err := store.SQL().QueryRowContext(ctx, `SELECT address_slot FROM paperboat.control_sync_leases WHERE machine_id=$1`, first.ID).Scan(&leaseSlot); err != nil {
		t.Fatalf("new machine has no local-address lease: %v", err)
	}
	var canonicalScope bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT m.environment_id=m.id AND e.id=m.id AND e.workspace_id=m.id AND e.owner_user_id=m.user_id FROM paperboat.user_machines m JOIN paperboat.control_environments e ON e.id=m.environment_id WHERE m.id=$1`, first.ID).Scan(&canonicalScope); err != nil || !canonicalScope || first.EnvironmentID != first.ID {
		t.Fatalf("canonical machine control scope = %v, %v", canonicalScope, err)
	}
	if first.Alias != "studio" {
		t.Fatalf("first alias=%q", first.Alias)
	}
	if first.Availability.DesiredMode != "allow_sleep" {
		t.Fatalf("default availability mode=%q, want allow_sleep", first.Availability.DesiredMode)
	}
	if !first.Capabilities.TerminalHost.Configured || !first.MachineCapabilities.Desired.ManagedSSH {
		t.Fatalf("unified defaults: %+v", first.MachineCapabilities)
	}
	selection := first.MachineCapabilities.Desired
	selection.Terminal = false
	if _, err := service.SetMachineCapabilities(ctx, userID, first.ID, "setup-capability-"+suffix, selection, first.MachineCapabilities.DesiredVersion); err != nil {
		t.Fatal(err)
	}

	var firstVersion int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT version FROM paperboat.user_machines WHERE id=$1`, first.ID).Scan(&firstVersion); err != nil {
		t.Fatal(err)
	}
	second, err := service.Setup(ctx, userID, input)
	if err != nil {
		t.Fatal(err)
	}
	var secondVersion, machineCount int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT version FROM paperboat.user_machines WHERE id=$1`, first.ID).Scan(&secondVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machines WHERE public_identity_key=$1`, input.PublicIdentityKey).Scan(&machineCount); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || secondVersion != firstVersion || machineCount != 1 || second.MachineCapabilities.Desired.Terminal {
		t.Fatalf("first=%+v second=%+v versions=%d/%d count=%d", first, second, firstVersion, secondVersion, machineCount)
	}
	keepAwakeInput := input
	keepAwakeInput.KeepAwake = true
	keptAwake, err := service.Setup(ctx, userID, keepAwakeInput)
	if err != nil || keptAwake.Availability.DesiredMode != "keep_awake" {
		t.Fatalf("keep-awake opt-in machine=%+v err=%v", keptAwake, err)
	}
	preserved, err := service.Setup(ctx, userID, input)
	if err != nil || preserved.Availability.DesiredMode != "keep_awake" {
		t.Fatalf("omitted preference reset existing policy: machine=%+v err=%v", preserved, err)
	}
	replacementPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	replacement := input
	replacement.PublicIdentityKey = base64.RawURLEncoding.EncodeToString(replacementPublic)
	if _, err := service.Setup(ctx, userID, replacement); !errors.Is(err, ErrMachineNameConflict) {
		t.Fatalf("replacement setup error = %v, want ErrMachineNameConflict", err)
	}
	collisionPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	collision := input
	collision.Alias = "Studio!!!"
	collision.PublicIdentityKey = base64.RawURLEncoding.EncodeToString(collisionPublic)
	if _, err := service.Setup(ctx, userID, collision); !errors.Is(err, ErrInvalidSetup) {
		t.Fatalf("invalid alias setup = %v", err)
	}
	collision.Alias = "studio-2"
	collidingMachine, err := service.Setup(ctx, userID, collision)
	if err != nil {
		t.Fatal(err)
	}
	if collidingMachine.Alias != "studio-2" {
		t.Fatalf("collision alias=%q", collidingMachine.Alias)
	}

}

func TestOnlineMachinePairingPreservesIdentity(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_client_upgrade_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "client-upgrade-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_client_upgrade_"+suffix, userID, "sub_client_upgrade_"+suffix); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := base64.RawURLEncoding.EncodeToString(public)
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("client-upgrade-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "henr_client_upgrade_" + suffix, HelperID: "helper_client_upgrade_" + suffix, Credential: "client-upgrade-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	machine, err := service.Setup(ctx, userID, SetupInput{Alias: "studio", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/paperboat", PublicIdentityKey: publicKey, RuntimeVersions: json.RawMessage(`{"pb":"test"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET state='online', online=true, observed_capabilities=configured_capabilities WHERE id=$1`, machine.ID); err != nil {
		t.Fatal(err)
	}
	pairing, err := service.CreatePairing(ctx, PairingInput{Verifier: "client-upgrade-verifier-" + suffix, Alias: "studio", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/paperboat", PublicIdentityKey: publicKey, RuntimeVersions: json.RawMessage(`{"pb":"test"}`), KeepAwake: true})
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := service.Approve(ctx, userID, pairing.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.ID != machine.ID || upgraded.SeatState != "occupied" || !upgraded.Capabilities.TerminalHost.Configured {
		t.Fatalf("upgraded=%+v", upgraded)
	}
	if upgraded.Availability.DesiredMode != "keep_awake" {
		t.Fatalf("pairing keep-awake mode=%q, want keep_awake", upgraded.Availability.DesiredMode)
	}
}

func TestAuthenticatedMachineInstallationIsBoundIdempotentAndGenerationFenced(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, clientSessionID := "usr_authenticated_host_"+suffix, "cls_authenticated_host_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "authenticated-host-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,$3,'Machine installation test','desktop','linux',ARRAY['machines:connect'],'active',$4,$4)`, clientSessionID, userID, "client_"+suffix, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_authenticated_host_"+suffix, userID, "sub_authenticated_host_"+suffix); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := base64.RawURLEncoding.EncodeToString(public)
	setupSigner, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	setupEnrollments := controlplane.NewEnrollmentService(store, setupSigner, audit.NewWriter(store), "https://api.paperboat.test", "authenticated-host-setup-enrollment-key")
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("authenticated-host-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureCLIAuth("paperboat-cli", []string{"account:read", "machines:connect", "session:refresh"}, 5*time.Minute, 24*time.Hour, "authenticated-host-cli-hash-key")
	service.ConfigureHelperEnrollment(func(_ context.Context, gotUserID, operationKey, environmentID string, _ time.Duration) (HelperEnrollmentGrant, error) {
		if gotUserID != userID || !strings.Contains(operationKey, clientSessionID) || environmentID == "" {
			t.Fatalf("helper grant binding user=%q operation=%q environment=%q", gotUserID, operationKey, environmentID)
		}
		return HelperEnrollmentGrant{EnrollmentID: "henr_authenticated_" + suffix, HelperID: "helper_authenticated_" + suffix, Credential: strings.Repeat("c", 48), ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	service.ConfigureAuthenticatedHelperEnrollment(
		func(ctx context.Context, gotUserID, operationKey, environmentID string, lifetime time.Duration, guard HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			if gotUserID != userID || !strings.HasPrefix(operationKey, "authenticated-machine-installation:pairing_") || environmentID == "" {
				return HelperEnrollmentGrant{}, fmt.Errorf("helper grant binding user=%q operation=%q environment=%q", gotUserID, operationKey, environmentID)
			}
			return PersistAuthenticatedHelperEnrollment(ctx, store, environmentID, guard, func(ctx context.Context) (HelperEnrollmentGrant, error) {
				grant, issueErr := setupEnrollments.Issue(ctx, gotUserID, operationKey, environmentID, lifetime)
				return HelperEnrollmentGrant{EnrollmentID: grant.EnrollmentID, HelperID: grant.HelperID, Credential: grant.Credential, ExpiresAt: grant.ExpiresAt}, issueErr
			})
		},
		func(context.Context, string, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			return HelperEnrollmentGrant{}, errors.New("unexpected authenticated helper recovery")
		},
	)
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	input := SetupInput{Alias: "studio", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/paperboat", PublicIdentityKey: publicKey, RuntimeVersions: json.RawMessage(`{"pb":"test"}`)}
	clientMachine, err := service.Setup(ctx, userID, input)
	if err != nil {
		t.Fatal(err)
	}
	hostMachine, err := service.Setup(ctx, userID, input)
	if err != nil {
		t.Fatal(err)
	}
	if hostMachine.ID != clientMachine.ID || hostMachine.SeatState != "occupied" || hostMachine.InstallationGeneration != clientMachine.InstallationGeneration || hostMachine.Installation == nil {
		t.Fatalf("authenticated Host transition = %+v, previous = %+v", hostMachine, clientMachine)
	}
	if hostMachine.Availability.DesiredMode != "allow_sleep" {
		t.Fatalf("Machine installation availability=%q, want allow_sleep before installation opt-in", hostMachine.Availability.DesiredMode)
	}
	verifier := "authenticated-host-verifier-" + suffix
	request := AuthenticatedMachineInstallationInput{OperationID: "host-setup-operation-" + suffix, Verifier: verifier, PublicIdentityKey: publicKey, InstallationGeneration: hostMachine.InstallationGeneration, KeepAwake: true}
	first, err := service.PrepareAuthenticatedMachineInstallation(ctx, userID, clientSessionID, hostMachine.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	var preparedAvailability string
	if err := store.SQL().QueryRowContext(ctx, `SELECT availability_mode FROM paperboat.user_machines WHERE id=$1`, hostMachine.ID).Scan(&preparedAvailability); err != nil || preparedAvailability != "keep_awake" {
		t.Fatalf("prepared Host availability=%q err=%v, want keep_awake", preparedAvailability, err)
	}
	second, err := service.PrepareAuthenticatedMachineInstallation(ctx, userID, clientSessionID, hostMachine.ID, request)
	if err != nil || second.ExpiresAt != first.ExpiresAt {
		t.Fatalf("idempotent prepare = %+v, err=%v; first=%+v", second, err, first)
	}
	changedPreference := request
	changedPreference.KeepAwake = false
	if _, err := service.PrepareAuthenticatedMachineInstallation(ctx, userID, clientSessionID, hostMachine.ID, changedPreference); !errors.Is(err, ErrMachineInstallationOperationConflict) {
		t.Fatalf("changed keep-awake preference error=%v, want ErrMachineInstallationOperationConflict", err)
	}
	var pairingCount int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_pairings WHERE authenticated_setup_cli_session_id=$1 AND authenticated_setup_operation_id=$2`, clientSessionID, request.OperationID).Scan(&pairingCount); err != nil || pairingCount != 1 {
		t.Fatalf("authenticated pairing count=%d err=%v", pairingCount, err)
	}
	var pairingKeepAwake bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT keep_awake FROM paperboat.user_machine_pairings WHERE authenticated_setup_cli_session_id=$1 AND authenticated_setup_operation_id=$2`, clientSessionID, request.OperationID).Scan(&pairingKeepAwake); err != nil || !pairingKeepAwake {
		t.Fatalf("authenticated pairing keep_awake=%t err=%v", pairingKeepAwake, err)
	}
	if _, err := service.ConsumeInstallation(ctx, verifier); !errors.Is(err, ErrInstallationUnavailable) {
		t.Fatalf("authenticated Host material accepted a legacy identity-less consume: %v", err)
	}
	material, err := service.ConsumeInstallationForIdentity(ctx, verifier, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		MachineID     string                    `json:"user_machine_id"`
		Generation    int64                     `json:"installation_generation"`
		ClientSession installationClientSession `json:"client_session"`
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(material, &wire); err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"setup_mode", "setup_roles", "artifact"} {
		if _, ok := wire[retired]; ok {
			t.Fatalf("installation exposes retired field %s", retired)
		}
	}
	if err := json.Unmarshal(material, &fields); err != nil || fields.MachineID != hostMachine.ID || fields.Generation != hostMachine.InstallationGeneration {
		t.Fatalf("installation identity mismatch: machine=%s generation=%d err=%v", fields.MachineID, fields.Generation, err)
	}
	if fields.ClientSession.SessionID == "" || fields.ClientSession.AccessToken == "" || fields.ClientSession.RefreshToken == "" {
		t.Fatalf("authenticated Host client session is incomplete: %+v", fields.ClientSession)
	}
	bootstrapMachineAuth := authservice.NewDeviceService(store, audit.NewWriter(store), config.Default().CLIAuth, []string{"authenticated-host-cli-hash-key"})
	principal, err := bootstrapMachineAuth.Authenticate(ctx, fields.ClientSession.AccessToken)
	if err != nil || principal.SessionID != fields.ClientSession.SessionID || principal.User.ID != userID {
		t.Fatalf("authenticated Host bootstrap CLI access token rejected: principal=%+v err=%v", principal, err)
	}
	var sessionState string
	var freshBootstrap bool
	var boundMachine sql.NullString
	var accessTokens, refreshTokens int
	if err := store.SQL().QueryRowContext(ctx, `SELECT state, fresh_e2ee_bootstrap, user_machine_id FROM paperboat.cli_client_sessions WHERE id=$1`, fields.ClientSession.SessionID).Scan(&sessionState, &freshBootstrap, &boundMachine); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.cli_access_tokens WHERE cli_client_session_id=$1), (SELECT count(*) FROM paperboat.cli_refresh_tokens WHERE cli_client_session_id=$1)`, fields.ClientSession.SessionID).Scan(&accessTokens, &refreshTokens); err != nil {
		t.Fatal(err)
	}
	if sessionState != "active" || !freshBootstrap || !boundMachine.Valid || boundMachine.String != hostMachine.ID || accessTokens != 1 || refreshTokens != 1 {
		t.Fatalf("authenticated Host bootstrap CLI session state=%q fresh=%v machine=%v access_tokens=%d refresh_tokens=%d", sessionState, freshBootstrap, boundMachine, accessTokens, refreshTokens)
	}
	conflict := request
	conflict.Verifier += "-different"
	if _, err := service.PrepareAuthenticatedMachineInstallation(ctx, userID, clientSessionID, hostMachine.ID, conflict); !errors.Is(err, ErrMachineInstallationOperationConflict) {
		t.Fatalf("operation conflict error=%v", err)
	}

	rolledBack, err := invalidateTestInstallation(ctx, service, userID, hostMachine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.SeatState != "occupied" || rolledBack.InstallationGeneration != hostMachine.InstallationGeneration+1 {
		t.Fatalf("generation invalidation=%+v", rolledBack)
	}
	var pairingState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_pairings WHERE authenticated_setup_cli_session_id=$1 AND authenticated_setup_operation_id=$2`, clientSessionID, request.OperationID).Scan(&pairingState); err != nil || pairingState != "expired" {
		t.Fatalf("rolled-back authenticated pairing state=%q err=%v", pairingState, err)
	}
	if _, err := service.ConsumeInstallationForIdentity(ctx, verifier, publicKey); !errors.Is(err, ErrInstallationExpired) {
		t.Fatalf("rolled-back authenticated material remained recoverable: %v", err)
	}

	// Pairing creation and material persistence are separate short transactions
	// because helper enrollment can perform its own database work. Hold material
	// construction between them and prove a concurrent generation invalidation wins the second
	// lock, expires the pairing, and leaves no consumable Host authority.
	raceHost, err := service.Setup(ctx, userID, input)
	if err != nil {
		t.Fatal(err)
	}
	raceHelperID, raceEnrollmentID := "helper_authenticated_race_"+suffix, "henr_authenticated_race_"+suffix
	authorityPersisted, releaseAuthority := make(chan struct{}), make(chan struct{})
	var authorityPersistedOnce sync.Once
	service.ConfigureAuthenticatedHelperEnrollment(
		func(ctx context.Context, gotUserID, operationKey, environmentID string, _ time.Duration, guard HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			expiresAt := time.Now().UTC().Add(10 * time.Minute)
			err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				if err := guard(ctx, tx, ""); err != nil {
					return err
				}
				if _, err := tx.Queries().CreateControlHelper(ctx, dbsqlc.CreateControlHelperParams{ID: raceHelperID, EnvironmentID: environmentID}); err != nil {
					return err
				}
				if _, err := tx.Queries().CreateControlHelperEnrollment(ctx, dbsqlc.CreateControlHelperEnrollmentParams{
					ID: raceEnrollmentID, EnvironmentID: environmentID, HelperID: raceHelperID,
					JtiHash: []byte("race-jti-" + suffix), OperationKey: "helper-enrollment:" + gotUserID + ":" + operationKey,
					RequestHash: []byte("race-request-" + suffix), GrantCiphertext: []byte("race-grant-" + suffix), ExpiresAt: expiresAt,
				}); err != nil {
					return err
				}
				if err := guard(ctx, tx, raceEnrollmentID); err != nil {
					return err
				}
				authorityPersistedOnce.Do(func() { close(authorityPersisted) })
				select {
				case <-releaseAuthority:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			return HelperEnrollmentGrant{EnrollmentID: raceEnrollmentID, HelperID: raceHelperID, Credential: strings.Repeat("r", 48), ExpiresAt: expiresAt}, err
		},
		func(context.Context, string, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			return HelperEnrollmentGrant{}, errors.New("unexpected authenticated helper recovery")
		},
	)
	raceVerifier := "authenticated-host-race-verifier-" + suffix
	raceRequest := AuthenticatedMachineInstallationInput{OperationID: "host-setup-race-operation-" + suffix, Verifier: raceVerifier, PublicIdentityKey: publicKey, InstallationGeneration: raceHost.InstallationGeneration}
	type prepareResult struct {
		value AuthenticatedMachineInstallation
		err   error
	}
	raceStartedAt := time.Now()
	raceCtx, cancelRace := context.WithTimeout(ctx, 60*time.Second)
	defer cancelRace()
	var releaseAuthorityOnce sync.Once
	releaseInitialAuthority := func() { releaseAuthorityOnce.Do(func() { close(releaseAuthority) }) }
	defer releaseInitialAuthority()
	prepareDone := make(chan prepareResult, 1)
	go func() {
		value, prepareErr := service.PrepareAuthenticatedMachineInstallation(raceCtx, userID, clientSessionID, raceHost.ID, raceRequest)
		prepareDone <- prepareResult{value: value, err: prepareErr}
	}()
	select {
	case <-authorityPersisted:
	case <-raceCtx.Done():
		fatalWithPostgresLocks(t, store, "authenticated Host helper authority was not persisted after %s: %v", time.Since(raceStartedAt), raceCtx.Err())
	}
	authorityPersistedAt := time.Now()
	type rollbackResult struct {
		value UserMachine
		err   error
	}
	rollbackDone := make(chan rollbackResult, 1)
	go func() {
		value, rollbackErr := invalidateTestInstallation(raceCtx, service, userID, raceHost.ID)
		rollbackDone <- rollbackResult{value: value, err: rollbackErr}
	}()
	if err := waitForUserMachineRowLock(raceCtx, store, raceHost.ID); err != nil {
		fatalWithPostgresLocks(t, store, "generation invalidation did not acquire the machine row before authority release after %s: %v", time.Since(authorityPersistedAt), err)
	}
	rollbackLockedAt := time.Now()
	releaseInitialAuthority()
	authorityReleasedAt := time.Now()
	postReleaseCtx, cancelPostRelease := context.WithTimeout(raceCtx, 30*time.Second)
	defer cancelPostRelease()
	var rolledBackConcurrently rollbackResult
	select {
	case rolledBackConcurrently = <-rollbackDone:
	case <-postReleaseCtx.Done():
		fatalWithPostgresLocks(t, store, "concurrent generation invalidation did not finish within the post-release bound; authority=%s machine_lock=%s post_release=%s: %v", authorityPersistedAt.Sub(raceStartedAt), rollbackLockedAt.Sub(authorityPersistedAt), time.Since(authorityReleasedAt), postReleaseCtx.Err())
	}
	rollbackFinishedAt := time.Now()
	if rolledBackConcurrently.err != nil {
		t.Fatal(rolledBackConcurrently.err)
	}
	concurrentRollback := rolledBackConcurrently.value
	if concurrentRollback.InstallationGeneration != raceHost.InstallationGeneration+1 {
		t.Fatalf("concurrent generation invalidation=%+v", concurrentRollback)
	}
	var prepared prepareResult
	select {
	case prepared = <-prepareDone:
	case <-postReleaseCtx.Done():
		fatalWithPostgresLocks(t, store, "authenticated Host material construction did not finish within the post-release bound; rollback=%s post_release=%s: %v", rollbackFinishedAt.Sub(authorityReleasedAt), time.Since(authorityReleasedAt), postReleaseCtx.Err())
	}
	t.Logf("authenticated Host issuance race completed: authority=%s machine_lock=%s rollback_after_release=%s total_after_release=%s", authorityPersistedAt.Sub(raceStartedAt), rollbackLockedAt.Sub(authorityPersistedAt), rollbackFinishedAt.Sub(authorityReleasedAt), time.Since(authorityReleasedAt))
	if prepared.err != nil && !errors.Is(prepared.err, ErrInvalidMachineInstallation) && !errors.Is(prepared.err, ErrMachineInstallationOperationConflict) {
		t.Fatalf("stale Host issuance result=%+v err=%v", prepared.value, prepared.err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_pairings WHERE authenticated_setup_cli_session_id=$1 AND authenticated_setup_operation_id=$2`, clientSessionID, raceRequest.OperationID).Scan(&pairingState); err != nil || pairingState != "expired" {
		t.Fatalf("concurrent rollback pairing state=%q err=%v", pairingState, err)
	}
	if _, err := service.ConsumeInstallationForIdentity(ctx, raceVerifier, publicKey); !errors.Is(err, ErrInstallationExpired) {
		t.Fatalf("concurrently rolled-back Host material remained consumable: %v", err)
	}
	var grantState string
	var grantRevokedAt sql.NullTime
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,revoked_at FROM paperboat.control_helper_enrollments WHERE id=$1 AND helper_id=$2`, raceEnrollmentID, raceHelperID).Scan(&grantState, &grantRevokedAt); err != nil || grantState != "revoked" || !grantRevokedAt.Valid {
		t.Fatalf("concurrently rolled-back helper grant state=%q revoked_at=%v err=%v", grantState, grantRevokedAt, err)
	}

	// A consumed authenticated setup may recover material after a lost response.
	// Persist a real replacement grant under the recovery guard, race a Client
	// downgrade against it, and prove neither that grant nor refreshed material
	// survives the downgrade.
	recoveryHost, err := service.Setup(ctx, userID, input)
	if err != nil {
		t.Fatal(err)
	}
	var recoverySeedGrant HelperEnrollmentGrant
	service.ConfigureAuthenticatedHelperEnrollment(
		func(ctx context.Context, gotUserID, operationKey, environmentID string, lifetime time.Duration, guard HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			grant, issueErr := PersistAuthenticatedHelperEnrollment(ctx, store, environmentID, guard, func(ctx context.Context) (HelperEnrollmentGrant, error) {
				issued, err := setupEnrollments.Issue(ctx, gotUserID, operationKey, environmentID, lifetime)
				return HelperEnrollmentGrant{EnrollmentID: issued.EnrollmentID, HelperID: issued.HelperID, Credential: issued.Credential, ExpiresAt: issued.ExpiresAt}, err
			})
			if issueErr == nil {
				recoverySeedGrant = grant
			}
			return grant, issueErr
		},
		func(context.Context, string, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			return HelperEnrollmentGrant{}, errors.New("unexpected authenticated helper recovery")
		},
	)
	recoveryVerifier := "authenticated-host-recovery-verifier-" + suffix
	recoveryRequest := AuthenticatedMachineInstallationInput{OperationID: "host-setup-recovery-operation-" + suffix, Verifier: recoveryVerifier, PublicIdentityKey: publicKey, InstallationGeneration: recoveryHost.InstallationGeneration}
	if _, err := service.PrepareAuthenticatedMachineInstallation(ctx, userID, clientSessionID, recoveryHost.ID, recoveryRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeInstallationForIdentity(ctx, recoveryVerifier, publicKey); err != nil {
		t.Fatal(err)
	}
	if recoverySeedGrant.EnrollmentID == "" || recoverySeedGrant.HelperID == "" {
		t.Fatalf("authenticated recovery seed grant=%+v", recoverySeedGrant)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_helper_enrollments SET state='revoked',revoked_at=now() WHERE id=$1 AND state='pending'`, recoverySeedGrant.EnrollmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET expires_at=now()-interval '1 second' WHERE authenticated_setup_cli_session_id=$1 AND authenticated_setup_operation_id=$2`, clientSessionID, recoveryRequest.OperationID); err != nil {
		t.Fatal(err)
	}
	recoveryEnrollmentID := "henr_authenticated_recovery_race_" + suffix
	recoveryAuthorityPersisted, releaseRecoveryAuthority := make(chan struct{}), make(chan struct{})
	var recoveryAuthorityPersistedOnce sync.Once
	service.ConfigureAuthenticatedHelperEnrollment(
		func(ctx context.Context, gotUserID, operationKey, environmentID string, _ time.Duration, guard HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			expiresAt := time.Now().UTC().Add(10 * time.Minute)
			err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				if err := guard(ctx, tx, ""); err != nil {
					return err
				}
				if _, err := tx.Queries().CreateControlHelperEnrollment(ctx, dbsqlc.CreateControlHelperEnrollmentParams{
					ID: recoveryEnrollmentID, EnvironmentID: environmentID, HelperID: recoverySeedGrant.HelperID,
					JtiHash: []byte("recovery-race-jti-" + suffix), OperationKey: "helper-enrollment:" + gotUserID + ":" + operationKey,
					RequestHash: []byte("recovery-race-request-" + suffix), GrantCiphertext: []byte("recovery-race-grant-" + suffix), ExpiresAt: expiresAt,
				}); err != nil {
					return err
				}
				if err := guard(ctx, tx, recoveryEnrollmentID); err != nil {
					return err
				}
				recoveryAuthorityPersistedOnce.Do(func() { close(recoveryAuthorityPersisted) })
				select {
				case <-releaseRecoveryAuthority:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			return HelperEnrollmentGrant{EnrollmentID: recoveryEnrollmentID, HelperID: recoverySeedGrant.HelperID, Credential: strings.Repeat("z", 48), ExpiresAt: expiresAt}, err
		},
		func(context.Context, string, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			return HelperEnrollmentGrant{}, errors.New("unexpected authenticated helper recovery")
		},
	)
	type recoveryResult struct {
		material json.RawMessage
		err      error
	}
	recoveryStartedAt := time.Now()
	recoveryCtx, cancelRecovery := context.WithTimeout(ctx, 60*time.Second)
	defer cancelRecovery()
	var releaseRecoveryAuthorityOnce sync.Once
	releaseRecoveredAuthority := func() { releaseRecoveryAuthorityOnce.Do(func() { close(releaseRecoveryAuthority) }) }
	defer releaseRecoveredAuthority()
	recoveryDone := make(chan recoveryResult, 1)
	go func() {
		material, recoveryErr := service.ConsumeInstallationForIdentityState(recoveryCtx, recoveryVerifier, publicKey, false)
		recoveryDone <- recoveryResult{material: material, err: recoveryErr}
	}()
	select {
	case <-recoveryAuthorityPersisted:
	case <-recoveryCtx.Done():
		fatalWithPostgresLocks(t, store, "authenticated Host recovery helper authority was not persisted after %s: %v", time.Since(recoveryStartedAt), recoveryCtx.Err())
	}
	recoveryAuthorityPersistedAt := time.Now()
	recoveryRollbackDone := make(chan rollbackResult, 1)
	go func() {
		value, rollbackErr := invalidateTestInstallation(recoveryCtx, service, userID, recoveryHost.ID)
		recoveryRollbackDone <- rollbackResult{value: value, err: rollbackErr}
	}()
	if err := waitForUserMachineRowLock(recoveryCtx, store, recoveryHost.ID); err != nil {
		fatalWithPostgresLocks(t, store, "generation invalidation did not acquire the machine row before recovery authority release after %s: %v", time.Since(recoveryAuthorityPersistedAt), err)
	}
	recoveryRollbackLockedAt := time.Now()
	releaseRecoveredAuthority()
	recoveryAuthorityReleasedAt := time.Now()
	recoveryPostReleaseCtx, cancelRecoveryPostRelease := context.WithTimeout(recoveryCtx, 30*time.Second)
	defer cancelRecoveryPostRelease()
	var recovered recoveryResult
	select {
	case recovered = <-recoveryDone:
	case <-recoveryPostReleaseCtx.Done():
		fatalWithPostgresLocks(t, store, "authenticated Host material recovery did not finish within the post-release bound; authority=%s machine_lock=%s post_release=%s: %v", recoveryAuthorityPersistedAt.Sub(recoveryStartedAt), recoveryRollbackLockedAt.Sub(recoveryAuthorityPersistedAt), time.Since(recoveryAuthorityReleasedAt), recoveryPostReleaseCtx.Err())
	}
	recoveryFinishedAt := time.Now()
	if recovered.err != nil && !errors.Is(recovered.err, ErrInstallationUnavailable) {
		t.Fatalf("stale authenticated Host recovery err=%v material=%s", recovered.err, recovered.material)
	}
	select {
	case rolledBackConcurrently = <-recoveryRollbackDone:
	case <-recoveryPostReleaseCtx.Done():
		fatalWithPostgresLocks(t, store, "generation invalidation racing authenticated recovery did not finish within the post-release bound; recovery=%s post_release=%s: %v", recoveryFinishedAt.Sub(recoveryAuthorityReleasedAt), time.Since(recoveryAuthorityReleasedAt), recoveryPostReleaseCtx.Err())
	}
	t.Logf("authenticated Host recovery race completed: authority=%s machine_lock=%s recovery_after_release=%s total_after_release=%s", recoveryAuthorityPersistedAt.Sub(recoveryStartedAt), recoveryRollbackLockedAt.Sub(recoveryAuthorityPersistedAt), recoveryFinishedAt.Sub(recoveryAuthorityReleasedAt), time.Since(recoveryAuthorityReleasedAt))
	if rolledBackConcurrently.err != nil || rolledBackConcurrently.value.InstallationGeneration != recoveryHost.InstallationGeneration+1 {
		t.Fatalf("generation invalidation racing authenticated recovery=%+v err=%v", rolledBackConcurrently.value, rolledBackConcurrently.err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_pairings WHERE authenticated_setup_cli_session_id=$1 AND authenticated_setup_operation_id=$2`, clientSessionID, recoveryRequest.OperationID).Scan(&pairingState); err != nil || pairingState != "expired" {
		t.Fatalf("recovery-race pairing state=%q err=%v", pairingState, err)
	}
	if _, err := service.ConsumeInstallationForIdentity(ctx, recoveryVerifier, publicKey); !errors.Is(err, ErrInstallationExpired) {
		t.Fatalf("rolled-back recovered Host material remained consumable: %v", err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,revoked_at FROM paperboat.control_helper_enrollments WHERE id=$1 AND helper_id=$2`, recoveryEnrollmentID, recoverySeedGrant.HelperID).Scan(&grantState, &grantRevokedAt); err != nil || grantState != "revoked" || !grantRevokedAt.Valid {
		t.Fatalf("rolled-back recovery helper grant state=%q revoked_at=%v err=%v", grantState, grantRevokedAt, err)
	}

	// The authenticated CLI session is part of the authority binding. Pause
	// after the helper credential is minted but before it is bound, queue session
	// revocation on the pairing lock, then prove the exact disclosed credential
	// is revoked before either operation returns authority to the caller.
	sessionHost, err := service.Setup(ctx, userID, input)
	if err != nil {
		t.Fatal(err)
	}
	sessionSigner, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sessionEnrollmentService := controlplane.NewEnrollmentService(store, sessionSigner, audit.NewWriter(store), "https://api.paperboat.test", "authenticated-session-enrollment-key")
	var sessionGrant controlplane.EnrollmentGrant
	sessionGrantPersisted, releaseSessionGrant := make(chan struct{}), make(chan struct{})
	var sessionGrantPersistedOnce, releaseSessionGrantOnce sync.Once
	releasePersistedSessionGrant := func() { releaseSessionGrantOnce.Do(func() { close(releaseSessionGrant) }) }
	defer releasePersistedSessionGrant()
	service.ConfigureAuthenticatedHelperEnrollment(
		func(ctx context.Context, gotUserID, operationKey, environmentID string, lifetime time.Duration, guard HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			return PersistAuthenticatedHelperEnrollment(ctx, store, environmentID, guard, func(ctx context.Context) (HelperEnrollmentGrant, error) {
				grant, err := sessionEnrollmentService.Issue(ctx, gotUserID, operationKey, environmentID, lifetime)
				if err != nil {
					return HelperEnrollmentGrant{}, err
				}
				sessionGrant = grant
				sessionGrantPersistedOnce.Do(func() { close(sessionGrantPersisted) })
				select {
				case <-releaseSessionGrant:
					return HelperEnrollmentGrant{EnrollmentID: grant.EnrollmentID, HelperID: grant.HelperID, Credential: grant.Credential, ExpiresAt: grant.ExpiresAt}, nil
				case <-ctx.Done():
					return HelperEnrollmentGrant{}, ctx.Err()
				}
			})
		},
		func(context.Context, string, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error) {
			return HelperEnrollmentGrant{}, errors.New("unexpected authenticated helper recovery")
		},
	)
	sessionVerifier := "authenticated-host-session-verifier-" + suffix
	sessionRequest := AuthenticatedMachineInstallationInput{OperationID: "host-setup-session-operation-" + suffix, Verifier: sessionVerifier, PublicIdentityKey: publicKey, InstallationGeneration: sessionHost.InstallationGeneration}
	sessionCtx, cancelSessionRace := context.WithTimeout(ctx, 60*time.Second)
	defer cancelSessionRace()
	sessionPrepareDone := make(chan prepareResult, 1)
	go func() {
		value, prepareErr := service.PrepareAuthenticatedMachineInstallation(sessionCtx, userID, clientSessionID, sessionHost.ID, sessionRequest)
		sessionPrepareDone <- prepareResult{value: value, err: prepareErr}
	}()
	select {
	case <-sessionGrantPersisted:
	case <-sessionCtx.Done():
		fatalWithPostgresLocks(t, store, "authenticated Host session-race grant was not persisted: %v", sessionCtx.Err())
	}
	machineAuth := authservice.NewDeviceService(store, audit.NewWriter(store), config.Default().CLIAuth, []string{"authenticated-host-session-hash-key"})
	sessionRevokeDone := make(chan error, 1)
	go func() {
		sessionRevokeDone <- machineAuth.RevokeClient(sessionCtx, userID, clientSessionID, "authenticated_host_test")
	}()
	if err := waitForCLIClientSessionRowLock(sessionCtx, store, clientSessionID); err != nil {
		fatalWithPostgresLocks(t, store, "CLI session revocation did not acquire the session row before helper binding: %v", err)
	}
	releasePersistedSessionGrant()
	select {
	case err := <-sessionRevokeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-sessionCtx.Done():
		fatalWithPostgresLocks(t, store, "CLI session revocation did not finish after helper binding: %v", sessionCtx.Err())
	}
	select {
	case prepared := <-sessionPrepareDone:
		if prepared.err != nil && !errors.Is(prepared.err, ErrInvalidMachineInstallation) && !errors.Is(prepared.err, ErrMachineInstallationOperationConflict) {
			t.Fatalf("session-raced Host preparation=%+v err=%v", prepared.value, prepared.err)
		}
	case <-sessionCtx.Done():
		fatalWithPostgresLocks(t, store, "authenticated Host preparation did not finish after CLI session revocation: %v", sessionCtx.Err())
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_pairings WHERE authenticated_setup_cli_session_id=$1 AND authenticated_setup_operation_id=$2`, clientSessionID, sessionRequest.OperationID).Scan(&pairingState); err != nil || pairingState != "expired" {
		t.Fatalf("CLI session revocation pairing state=%q err=%v", pairingState, err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,revoked_at FROM paperboat.control_helper_enrollments WHERE id=$1`, sessionGrant.EnrollmentID).Scan(&grantState, &grantRevokedAt); err != nil || grantState != "revoked" || !grantRevokedAt.Valid {
		t.Fatalf("CLI-session-revoked helper grant state=%q revoked_at=%v err=%v", grantState, grantRevokedAt, err)
	}
	_, sessionHelperPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionEnrollmentService.Exchange(ctx, sessionGrant.Credential, sessionHelperPrivate.Public().(ed25519.PublicKey)); !errors.Is(err, controlplane.ErrEnrollmentUsed) {
		t.Fatalf("CLI-session-revoked helper credential remained exchangeable: %v", err)
	}
	if _, err := service.ConsumeInstallationForIdentity(ctx, sessionVerifier, publicKey); !errors.Is(err, ErrInstallationExpired) {
		t.Fatalf("CLI-session-revoked Host material remained consumable: %v", err)
	}
}

func TestPersistAuthenticatedHelperEnrollmentCompensatesBindFailure(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_authenticated_compensation_" + suffix
	machineID := "mch_authenticated_compensation_" + suffix
	environmentID := "env_authenticated_compensation_" + suffix
	sessionID := "cls_authenticated_compensation_" + suffix
	pairingID := "ump_authenticated_compensation_" + suffix
	publicKeyHash := sha256.Sum256([]byte("authenticated-compensation-public-key-" + suffix))
	publicKey := base64.RawURLEncoding.EncodeToString(publicKeyHash[:])
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_authenticated_compensation_"+suffix, "authenticated-compensation-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,$3,'Host compensation','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, sessionID, userID, "client_authenticated_compensation_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','offline','occupied',$4,2)`, machineID, userID, environmentID, publicKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, environmentID, machineID, userID); err != nil {
		t.Fatal(err)
	}
	verifierHash := sha256.Sum256([]byte("authenticated-compensation-verifier-" + suffix))
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_pairings (
id,verifier_hash,user_code,requested_alias,platform,architecture,workspace_root,runtime_versions,state,approved_by_user_id,user_machine_id,public_identity_key,expires_at,approved_at,
authenticated_setup_cli_session_id,authenticated_setup_operation_id,authenticated_setup_generation,installation_state,generation) VALUES ($1,$2,$3,'Host compensation','linux','amd64','/workspace','{}','approved',$4,$5,$6,now()+interval '10 minutes',now(),$7,$8,2,'approved',1)`, pairingID, verifierHash[:], "COMP"+suffix, userID, machineID, publicKey, sessionID, "authenticated-compensation-operation-"+suffix); err != nil {
		t.Fatal(err)
	}
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollments := controlplane.NewEnrollmentService(store, signer, audit.NewWriter(store), "https://api.paperboat.test", "authenticated-compensation-enrollment-key")
	bindFailure := errors.New("injected authenticated Host helper bind failure")
	guard := func(ctx context.Context, tx *db.Tx, helperEnrollmentID string) error {
		pairing, err := tx.Queries().GetUserMachinePairingForVerifier(ctx, verifierHash[:])
		if err != nil {
			return err
		}
		if pairing.ID != pairingID || pairing.State != "approved" {
			return ErrMachineInstallationOperationConflict
		}
		if helperEnrollmentID != "" {
			return bindFailure
		}
		return nil
	}
	var issued controlplane.EnrollmentGrant
	_, err = PersistAuthenticatedHelperEnrollment(ctx, store, environmentID, guard, func(ctx context.Context) (HelperEnrollmentGrant, error) {
		grant, issueErr := enrollments.Issue(ctx, userID, "authenticated-compensation:"+suffix, environmentID, 10*time.Minute)
		issued = grant
		return HelperEnrollmentGrant{EnrollmentID: grant.EnrollmentID, HelperID: grant.HelperID, Credential: grant.Credential, ExpiresAt: grant.ExpiresAt}, issueErr
	})
	if !errors.Is(err, bindFailure) {
		t.Fatalf("authenticated helper bind failure=%v", err)
	}
	var enrollmentState string
	var revokedAt sql.NullTime
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,revoked_at FROM paperboat.control_helper_enrollments WHERE id=$1`, issued.EnrollmentID).Scan(&enrollmentState, &revokedAt); err != nil || enrollmentState != "revoked" || !revokedAt.Valid {
		t.Fatalf("compensated helper grant state=%q revoked_at=%v err=%v", enrollmentState, revokedAt, err)
	}
	var boundEnrollmentID sql.NullString
	if err := store.SQL().QueryRowContext(ctx, `SELECT authenticated_setup_helper_enrollment_id FROM paperboat.user_machine_pairings WHERE id=$1`, pairingID).Scan(&boundEnrollmentID); err != nil || boundEnrollmentID.Valid {
		t.Fatalf("failed helper grant remained bound=%v err=%v", boundEnrollmentID, err)
	}
	_, helperPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enrollments.Exchange(ctx, issued.Credential, helperPrivate.Public().(ed25519.PublicKey)); !errors.Is(err, controlplane.ErrEnrollmentUsed) {
		t.Fatalf("compensated helper credential remained exchangeable: %v", err)
	}
	foreignEnvironmentID := "env_authenticated_compensation_foreign_" + suffix
	foreignHelperID := "helper_authenticated_compensation_foreign_" + suffix
	foreignEnrollmentID := "henr_authenticated_compensation_foreign_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, foreignEnvironmentID, "workspace_authenticated_compensation_foreign_"+suffix, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id) VALUES ($1,$2)`, foreignHelperID, foreignEnvironmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helper_enrollments (id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,now()+interval '10 minutes')`, foreignEnrollmentID, foreignEnvironmentID, foreignHelperID, []byte("foreign-jti-"+suffix), "foreign-operation-"+suffix, []byte("foreign-request-"+suffix), []byte("foreign-grant-"+suffix)); err != nil {
		t.Fatal(err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		n, err := tx.Queries().BindAuthenticatedMachineInstallationHelperEnrollment(ctx, dbsqlc.BindAuthenticatedMachineInstallationHelperEnrollmentParams{ID: pairingID, HelperEnrollmentID: sql.NullString{String: foreignEnrollmentID, Valid: true}})
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("foreign-environment helper binding changed %d pairings", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	validEnrollmentID := "henr_authenticated_compensation_bound_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helper_enrollments (id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,now()+interval '10 minutes')`, validEnrollmentID, environmentID, issued.HelperID, []byte("bound-jti-"+suffix), "bound-operation-"+suffix, []byte("bound-request-"+suffix), []byte("bound-grant-"+suffix)); err != nil {
		t.Fatal(err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		n, err := tx.Queries().BindAuthenticatedMachineInstallationHelperEnrollment(ctx, dbsqlc.BindAuthenticatedMachineInstallationHelperEnrollmentParams{ID: pairingID, HelperEnrollmentID: sql.NullString{String: validEnrollmentID, Valid: true}})
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("same-environment helper binding changed %d pairings", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_helper_enrollments SET state='revoked',revoked_at=now() WHERE id=$1`, validEnrollmentID); err != nil {
		t.Fatal(err)
	}
	replacementEnrollmentID := "henr_authenticated_compensation_replacement_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helper_enrollments (id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,now()+interval '10 minutes')`, replacementEnrollmentID, environmentID, issued.HelperID, []byte("replacement-jti-"+suffix), "replacement-operation-"+suffix, []byte("replacement-request-"+suffix), []byte("replacement-grant-"+suffix)); err != nil {
		t.Fatal(err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		n, err := tx.Queries().BindAuthenticatedMachineInstallationHelperEnrollment(ctx, dbsqlc.BindAuthenticatedMachineInstallationHelperEnrollmentParams{ID: pairingID, HelperEnrollmentID: sql.NullString{String: replacementEnrollmentID, Valid: true}})
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("authenticated helper overwrite changed %d pairings", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET state='consumed',installation_config_consumed_at=now() WHERE id=$1`, pairingID); err != nil {
		t.Fatal(err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		n, err := tx.Queries().BindAuthenticatedMachineInstallationHelperEnrollment(ctx, dbsqlc.BindAuthenticatedMachineInstallationHelperEnrollmentParams{ID: pairingID, HelperEnrollmentID: sql.NullString{String: replacementEnrollmentID, Valid: true}})
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("authenticated helper replacement without an active recovery changed %d pairings", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET installation_recovery_operation_key=$2 WHERE id=$1`, pairingID, "authenticated-compensation-recovery-"+suffix); err != nil {
		t.Fatal(err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		n, err := tx.Queries().BindAuthenticatedMachineInstallationHelperEnrollment(ctx, dbsqlc.BindAuthenticatedMachineInstallationHelperEnrollmentParams{ID: pairingID, HelperEnrollmentID: sql.NullString{String: replacementEnrollmentID, Valid: true}})
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("authenticated recovery terminal helper replacement changed %d pairings", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var recoveredBinding sql.NullString
	if err := store.SQL().QueryRowContext(ctx, `SELECT authenticated_setup_helper_enrollment_id FROM paperboat.user_machine_pairings WHERE id=$1`, pairingID).Scan(&recoveredBinding); err != nil || !recoveredBinding.Valid || recoveredBinding.String != replacementEnrollmentID {
		t.Fatalf("authenticated recovery helper binding=%v err=%v", recoveredBinding, err)
	}
	competingPairingID := "ump_authenticated_compensation_competing_" + suffix
	competingVerifierHash := sha256.Sum256([]byte("authenticated-compensation-competing-verifier-" + suffix))
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_pairings (
id,verifier_hash,user_code,requested_alias,platform,architecture,workspace_root,runtime_versions,state,approved_by_user_id,user_machine_id,public_identity_key,expires_at,approved_at,
authenticated_setup_cli_session_id,authenticated_setup_operation_id,authenticated_setup_generation,installation_state,generation) VALUES ($1,$2,$3,'Host compensation','linux','amd64','/workspace','{}','approved',$4,$5,$6,now()+interval '10 minutes',now(),$7,$8,2,'approved',1)`, competingPairingID, competingVerifierHash[:], "COMQ"+suffix, userID, machineID, publicKey, sessionID, "authenticated-compensation-competing-operation-"+suffix); err != nil {
		t.Fatal(err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		n, err := tx.Queries().BindAuthenticatedMachineInstallationHelperEnrollment(ctx, dbsqlc.BindAuthenticatedMachineInstallationHelperEnrollmentParams{ID: competingPairingID, HelperEnrollmentID: sql.NullString{String: replacementEnrollmentID, Valid: true}})
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("one helper enrollment bound to %d competing authenticated pairings", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Queries().ExpireAuthenticatedMachineInstallationPairingsForCLISession(ctx, dbsqlc.ExpireAuthenticatedMachineInstallationPairingsForCLISessionParams{Now: time.Now().UTC(), CLIClientSessionID: sql.NullString{String: sessionID, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	var replacementState string
	var replacementRevokedAt sql.NullTime
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,revoked_at FROM paperboat.control_helper_enrollments WHERE id=$1`, replacementEnrollmentID).Scan(&replacementState, &replacementRevokedAt); err != nil || replacementState != "revoked" || !replacementRevokedAt.Valid {
		t.Fatalf("authenticated recovery replacement grant state=%q revoked_at=%v err=%v", replacementState, replacementRevokedAt, err)
	}
}

func TestEntitlementRevocationExpiresAuthenticatedHostGrant(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID, environmentID := "usr_entitlement_host_"+suffix, "mch_entitlement_host_"+suffix, "env_entitlement_host_"+suffix
	sessionID, pairingID := "cls_entitlement_host_"+suffix, "ump_entitlement_host_"+suffix
	publicKeyHash := sha256.Sum256([]byte("entitlement-host-public-key-" + suffix))
	publicKey := base64.RawURLEncoding.EncodeToString(publicKeyHash[:])
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_entitlement_host_"+suffix, "entitlement-host-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,$3,'Entitlement Host','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, sessionID, userID, "client_entitlement_host_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','offline','occupied',$4,2)`, machineID, userID, environmentID, publicKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, environmentID, machineID, userID); err != nil {
		t.Fatal(err)
	}
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollments := controlplane.NewEnrollmentService(store, signer, audit.NewWriter(store), "https://api.paperboat.test", "entitlement-host-enrollment-key")
	grant, err := enrollments.Issue(ctx, userID, "entitlement-host-grant:"+suffix, environmentID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	verifierHash := sha256.Sum256([]byte("entitlement-host-verifier-" + suffix))
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_pairings (
id,verifier_hash,user_code,requested_alias,platform,architecture,workspace_root,runtime_versions,state,approved_by_user_id,user_machine_id,public_identity_key,expires_at,approved_at,
authenticated_setup_cli_session_id,authenticated_setup_operation_id,authenticated_setup_generation,authenticated_setup_helper_enrollment_id
,installation_state,generation) VALUES ($1,$2,$3,'Entitlement Host','linux','amd64','/workspace','{}','approved',$4,$5,$6,now()+interval '10 minutes',now(),$7,$8,2,$9,'approved',1)`, pairingID, verifierHash[:], "ENTL"+suffix, userID, machineID, publicKey, sessionID, "entitlement-host-operation-"+suffix, grant.EnrollmentID); err != nil {
		t.Fatal(err)
	}
	revoked, err := store.Queries().RevokeUserMachinesForEntitlement(ctx, userID)
	if err != nil || !reflect.DeepEqual(revoked, []string{machineID}) {
		t.Fatalf("entitlement-revoked machines=%v err=%v", revoked, err)
	}
	var machineState, pairingState, grantState string
	var firstRevokedAt sql.NullTime
	if err := store.SQL().QueryRowContext(ctx, `SELECT m.state,p.state,e.state,e.revoked_at FROM paperboat.user_machines m JOIN paperboat.user_machine_pairings p ON p.user_machine_id=m.id JOIN paperboat.control_helper_enrollments e ON e.id=p.authenticated_setup_helper_enrollment_id WHERE m.id=$1`, machineID).Scan(&machineState, &pairingState, &grantState, &firstRevokedAt); err != nil {
		t.Fatal(err)
	}
	if machineState != "revoked" || pairingState != "expired" || grantState != "revoked" || !firstRevokedAt.Valid {
		t.Fatalf("entitlement revocation states machine=%q pairing=%q grant=%q revoked_at=%v", machineState, pairingState, grantState, firstRevokedAt)
	}
	_, helperPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enrollments.Exchange(ctx, grant.Credential, helperPrivate.Public().(ed25519.PublicKey)); !errors.Is(err, controlplane.ErrEnrollmentUsed) {
		t.Fatalf("entitlement-revoked helper credential remained exchangeable: %v", err)
	}
	if replay, err := store.Queries().RevokeUserMachinesForEntitlement(ctx, userID); err != nil || len(replay) != 0 {
		t.Fatalf("entitlement revocation replay=%v err=%v", replay, err)
	}
	var replayRevokedAt sql.NullTime
	if err := store.SQL().QueryRowContext(ctx, `SELECT revoked_at FROM paperboat.control_helper_enrollments WHERE id=$1`, grant.EnrollmentID).Scan(&replayRevokedAt); err != nil || !replayRevokedAt.Valid || !replayRevokedAt.Time.Equal(firstRevokedAt.Time) {
		t.Fatalf("entitlement revocation replay timestamp=%v first=%v err=%v", replayRevokedAt, firstRevokedAt, err)
	}
}

func TestRepeatedPairingPreservesExplicitCapabilitiesAndInstallationAuthority(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID, environmentID := "usr_client_pairing_host_"+suffix, "mch_client_pairing_host_"+suffix, "env_client_pairing_host_"+suffix
	sessionID, authenticatedPairingID := "cls_client_pairing_host_"+suffix, "ump_client_pairing_host_authenticated_"+suffix
	helperID, helperEnrollmentID := "hlp_client_pairing_host_"+suffix, "enr_client_pairing_host_"+suffix
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := base64.RawURLEncoding.EncodeToString(public)
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_client_pairing_host_"+suffix, "client-pairing-host-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,$3,'Client pairing Host rollback','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, sessionID, userID, "client_client_pairing_host_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_client_pairing_host_"+suffix, userID, "sub_client_pairing_host_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,runtime_versions,configured_capabilities,public_identity_key,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','offline','occupied','{}',ARRAY['file_receive','preview_launch','terminal_host','session_host','keep_awake'],$4,2)`, machineID, userID, environmentID, publicKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, environmentID, machineID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id) VALUES ($1,$2)`, helperID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helper_enrollments (id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,now()+interval '10 minutes')`, helperEnrollmentID, environmentID, helperID, []byte("client-pairing-host-jti-"+suffix), "client-pairing-host-grant-"+suffix, []byte("client-pairing-host-request-"+suffix), []byte("client-pairing-host-ciphertext-"+suffix)); err != nil {
		t.Fatal(err)
	}
	authenticatedVerifierHash := sha256.Sum256([]byte("client-pairing-host-authenticated-verifier-" + suffix))
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_pairings (
id,verifier_hash,user_code,requested_alias,platform,architecture,workspace_root,runtime_versions,state,approved_by_user_id,user_machine_id,public_identity_key,expires_at,approved_at,
authenticated_setup_cli_session_id,authenticated_setup_operation_id,authenticated_setup_generation,authenticated_setup_helper_enrollment_id
,installation_state,generation) VALUES ($1,$2,$3,'client-pairing-host','linux','amd64','/workspace','{}','approved',$4,$5,$6,now()+interval '10 minutes',now(),$7,$8,2,$9,'approved',1)`, authenticatedPairingID, authenticatedVerifierHash[:], "CPHA"+suffix, userID, machineID, publicKey, sessionID, "client-pairing-host-authenticated-operation-"+suffix, helperEnrollmentID); err != nil {
		t.Fatal(err)
	}
	legacyPairingID, legacyUserCode := "ump_client_pairing_host_legacy_"+suffix, "CPHL"+suffix
	legacyVerifierHash := sha256.Sum256([]byte("client-pairing-host-legacy-verifier-" + suffix))
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_pairings (id,verifier_hash,user_code,requested_alias,platform,architecture,workspace_root,runtime_versions,state,public_identity_key,expires_at,installation_state,generation) VALUES ($1,$2,$3,'client-pairing-host','linux','amd64','/workspace','{}','pending',$4,now()+interval '10 minutes','awaiting_approval',1)`, legacyPairingID, legacyVerifierHash[:], legacyUserCode, publicKey); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("client-pairing-host-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "enr_client_pairing_material_" + suffix, HelperID: "hlp_client_pairing_material_" + suffix, Credential: strings.Repeat("m", 48), ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := service.approve(ctx, userID, legacyUserCode)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.ID != machineID || rolledBack.SeatState != "occupied" || rolledBack.InstallationGeneration != 2 {
		t.Fatalf("repeated pairing machine=%+v", rolledBack)
	}
	var pairingState, grantState string
	var grantRevokedAt sql.NullTime
	if err := store.SQL().QueryRowContext(ctx, `SELECT pairing.state,enrollment.state,enrollment.revoked_at FROM paperboat.user_machine_pairings pairing JOIN paperboat.control_helper_enrollments enrollment ON enrollment.id=pairing.authenticated_setup_helper_enrollment_id WHERE pairing.id=$1`, authenticatedPairingID).Scan(&pairingState, &grantState, &grantRevokedAt); err != nil {
		t.Fatal(err)
	}
	if pairingState != "approved" || grantState != "pending" || grantRevokedAt.Valid {
		t.Fatalf("preserved installation authority state pairing=%q grant=%q revoked_at=%v", pairingState, grantState, grantRevokedAt)
	}
}

func TestWorkerMarksStaleMachineOfflineAndHeartbeatRestoresIt(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, userMachineID, environmentID := "usr_um_liveness_"+suffix, "um_liveness_"+suffix, "env_liveness_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "cm-liveness-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,last_seen_at) VALUES ($1,$2,$3,'linux','amd64','/home/test','online','occupied',true,now()-interval '1 minute')`, userMachineID, userID, environmentID); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{OfflineAfter: 10 * time.Second}, nil)
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Worker(5 * time.Millisecond)(workerCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		var state string
		var online bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT state,online FROM paperboat.user_machines WHERE id=$1`, userMachineID).Scan(&state, &online); err != nil {
			t.Fatal(err)
		}
		if state == "offline" && !online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stale machine remained state=%s online=%v", state, online)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if eligible, err := store.Queries().MarkUserMachineOnlineFromHelper(ctx, dbsqlc.MarkUserMachineOnlineFromHelperParams{ID: userMachineID, EnvironmentID: environmentID}); err != nil || !eligible {
		t.Fatalf("restore heartbeat eligible=%v err=%v", eligible, err)
	}
	var state string
	var online bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,online FROM paperboat.user_machines WHERE id=$1`, userMachineID).Scan(&state, &online); err != nil {
		t.Fatal(err)
	}
	if state != "online" || !online {
		t.Fatalf("restored machine state=%s online=%v", state, online)
	}
}

func TestCreateTerminalSessionMapsDuplicateNameToConflict(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID := "usr_um_session_"+suffix, "um_session_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "session-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES ($1,$2,$3,'linux','amd64','/home/test','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machineID, userID, "env_session_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	if _, err := service.CreateTerminalSession(ctx, userID, machineID, "benchmark", "create-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateTerminalSession(ctx, userID, machineID, "benchmark", "create-two"); !errors.Is(err, ErrTerminalSessionConflict) {
		t.Fatalf("duplicate name error = %v, want %v", err, ErrTerminalSessionConflict)
	}
}

func TestCreateTerminalSessionIsUnlimitedPerMachine(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID := "usr_um_unlimited_"+suffix, "um_unlimited_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "unlimited-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES ($1,$2,$3,'linux','amd64','/home/test','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machineID, userID, "env_unlimited_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	// Native terminals have no server-side count: 25 is above the old per-machine limit of 20.
	var first TerminalSession
	for i := range 25 {
		created, err := service.CreateTerminalSession(ctx, userID, machineID, "", fmt.Sprintf("unlimited-%d", i))
		if err != nil {
			t.Fatalf("session %d of 25: %v", i+1, err)
		}
		if i == 0 {
			first = created
		}
	}
	// Retries still resolve the original durable session.
	replay, err := service.CreateTerminalSession(ctx, userID, machineID, "", "unlimited-0")
	if err != nil || replay.ID != first.ID {
		t.Fatalf("idempotent replay = %+v, %v; want %s", replay, err, first.ID)
	}
	if rows := countTerminalSessionRows(t, store, machineID); rows != 25 {
		t.Fatalf("session records = %d, want 25", rows)
	}
	var queuedOperations int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_terminal_session_operations WHERE user_machine_id=$1`, machineID).Scan(&queuedOperations); err != nil {
		t.Fatal(err)
	}
	if queuedOperations != 25 {
		t.Fatalf("host creation operations = %d, want one per accepted session", queuedOperations)
	}
}

func countTerminalSessionRows(t *testing.T, store *db.DB, machineID string) int {
	t.Helper()
	var rows int
	if err := store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FROM paperboat.user_machine_terminal_sessions WHERE user_machine_id=$1`, machineID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestTerminalSessionRetentionExpiresOnlyConfirmedClosedRows(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID := "usr_um_retention_"+suffix, "um_retention_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_retention_"+suffix, "retention-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES ($1,$2,$3,'linux','amd64','/home/test','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machineID, userID, "env_retention_"+suffix); err != nil {
		t.Fatal(err)
	}
	oldClose := time.Now().UTC().Add(-31 * 24 * time.Hour)
	recentClose := time.Now().UTC().Add(-29 * 24 * time.Hour)
	ids := map[string]string{
		"expired": "umts_retention_expired_" + suffix,
		"recent":  "umts_retention_recent_" + suffix,
		"live":    "umts_retention_live_" + suffix,
		"ack":     "umts_retention_ack_" + suffix,
		"default": "umts_retention_default_" + suffix,
	}
	for name, value := range map[string]struct {
		state, desired string
		closedAt       sql.NullTime
		isDefault      bool
	}{
		"expired": {state: "closed", desired: "closed", closedAt: sql.NullTime{Time: oldClose, Valid: true}},
		"recent":  {state: "exited", desired: "closed", closedAt: sql.NullTime{Time: recentClose, Valid: true}},
		// The old timestamp is deliberately inconsistent with a live runtime
		// state. Cleanup must trust the current state and leave it untouched.
		"live":    {state: "running", desired: "open", closedAt: sql.NullTime{Time: oldClose, Valid: true}},
		"ack":     {state: "running", desired: "closed"},
		"default": {state: "closed", desired: "closed", closedAt: sql.NullTime{Time: oldClose, Valid: true}, isDefault: true},
	} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_terminal_sessions (id,user_machine_id,name,is_default,owner_account,launch_cwd,desired_state,runtime_state,closed_at,title) VALUES ($1,$2,$3,$4,$5,'/private/workspace',$6,$7,$8,'private title')`, ids[name], machineID, name, value.isDefault, userID, value.desired, value.state, value.closedAt); err != nil {
			t.Fatal(err)
		}
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	if err := store.Queries().MarkUserMachineTerminalSessionRuntimeClosed(ctx, ids["ack"]); err != nil {
		t.Fatal(err)
	}
	var acknowledgedState string
	var acknowledgedClosedAt sql.NullTime
	if err := store.SQL().QueryRowContext(ctx, `SELECT runtime_state,closed_at FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, ids["ack"]).Scan(&acknowledgedState, &acknowledgedClosedAt); err != nil {
		t.Fatal(err)
	}
	if acknowledgedState != "closed" || !acknowledgedClosedAt.Valid {
		t.Fatalf("close acknowledgement state=%q closed_at=%+v, want confirmed closed state and timestamp", acknowledgedState, acknowledgedClosedAt)
	}
	if err := service.expireClosedTerminalSessions(ctx); err != nil {
		t.Fatal(err)
	}
	var expiredDeleted, recentDeleted, liveDeleted, defaultDeleted bool
	var expiredTitle sql.NullString
	var expiredCWD string
	var defaultTitle sql.NullString
	var defaultCWD string
	for key, target := range map[string]*bool{"expired": &expiredDeleted, "recent": &recentDeleted, "live": &liveDeleted, "default": &defaultDeleted} {
		var title sql.NullString
		var cwd string
		if err := store.SQL().QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL,title,launch_cwd FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, ids[key]).Scan(target, &title, &cwd); err != nil {
			t.Fatal(err)
		}
		if key == "expired" {
			expiredTitle, expiredCWD = title, cwd
		} else if key == "default" {
			defaultTitle, defaultCWD = title, cwd
		}
	}
	if !expiredDeleted || expiredTitle.Valid || expiredCWD != "" {
		t.Fatalf("expired closed history cleanup deleted=%v title=%v cwd=%q; want deleted with private metadata scrubbed", expiredDeleted, expiredTitle, expiredCWD)
	}
	if recentDeleted || liveDeleted || defaultDeleted || !defaultTitle.Valid || defaultCWD != "/private/workspace" {
		t.Fatalf("cleanup changed protected records: recent deleted=%v, live deleted=%v, default deleted=%v title=%v cwd=%q", recentDeleted, liveDeleted, defaultDeleted, defaultTitle, defaultCWD)
	}
	var expiredPurgeCount int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_terminal_session_operations WHERE terminal_session_id=$1 AND operation='delete_history' AND state='pending'`, ids["expired"]).Scan(&expiredPurgeCount); err != nil {
		t.Fatal(err)
	}
	if expiredPurgeCount != 1 {
		t.Fatalf("expired host purge operation count = %d, want 1 pending delete_history", expiredPurgeCount)
	}

	if _, err := service.DeleteTerminalSession(ctx, userID, machineID, ids["live"]); !errors.Is(err, ErrTerminalSessionNotClosed) {
		t.Fatalf("owner deletion of live session = %v, want ErrTerminalSessionNotClosed", err)
	}
	var liveOperationCount int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_terminal_session_operations WHERE terminal_session_id=$1`, ids["live"]).Scan(&liveOperationCount); err != nil {
		t.Fatal(err)
	}
	if liveOperationCount != 0 {
		t.Fatalf("refused live-history deletion queued %d host operations, want 0", liveOperationCount)
	}

	// Owners can delete a confirmed-closed record before its scheduled expiry.
	applied, err := service.DeleteTerminalSession(ctx, userID, machineID, ids["recent"])
	if err != nil || applied {
		t.Fatalf("early owner deletion = applied %v, err %v; want a durable pending host purge", applied, err)
	}
	var ownerDeleted bool
	var ownerTitle sql.NullString
	if err := store.SQL().QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL,title FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, ids["recent"]).Scan(&ownerDeleted, &ownerTitle); err != nil {
		t.Fatal(err)
	}
	if !ownerDeleted || ownerTitle.Valid {
		t.Fatalf("early deletion metadata deleted=%v title=%v; want deleted and scrubbed", ownerDeleted, ownerTitle)
	}
	var purgeState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_terminal_session_operations WHERE terminal_session_id=$1 AND operation='delete_history'`, ids["recent"]).Scan(&purgeState); err != nil {
		t.Fatal(err)
	}
	if purgeState != "pending" {
		t.Fatalf("early host purge state = %q, want pending", purgeState)
	}
}

func TestCreateTerminalSessionAllocatesNameWhenOmitted(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID := "user_names_"+suffix, "machine_names_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, userID); err != nil {
		t.Fatal(err)
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, userID)
	for i, machine := range []string{machineID, machineID + "_other"} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES($1,$2,$1,$3,'linux','amd64','/home/test','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machine, userID, fmt.Sprintf("names-%s-%d", suffix, i)); err != nil {
			t.Fatal(err)
		}
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	// Deliberately repeat the first candidate on another machine, then redraw.
	service.nameRandom = bytes.NewReader([]byte{0, 0, 0, 0, 0, 0, 1, 1, 1, 2, 2, 2, 3, 3, 3})
	first, err := service.CreateTerminalSession(ctx, userID, machineID, "", "create-auto-one")
	if err != nil || first.Name != "agile-alpine-anchor" {
		t.Fatalf("first name %q %v", first.Name, err)
	}
	replay, err := service.CreateTerminalSession(ctx, userID, machineID, "", "create-auto-one")
	if err != nil || replay.ID != first.ID || replay.Name != first.Name {
		t.Fatalf("idempotent replay %+v %v", replay, err)
	}
	second, err := service.CreateTerminalSession(ctx, userID, machineID+"_other", "", "create-auto-two")
	if err != nil || second.Name != "alert-amber-arch" {
		t.Fatalf("cross-machine collision retry %+v %v", second, err)
	}
	claimed, err := service.CreateTerminalSession(ctx, userID, machineID, "bold-arctic-barn", "create-custom")
	if err != nil || claimed.Name != "bold-arctic-barn" {
		t.Fatalf("explicit name %+v %v", claimed, err)
	}
	automatic, err := service.CreateTerminalSession(ctx, userID, machineID, "", "create-auto-three")
	if err != nil || automatic.Name != "brave-autumn-bay" {
		t.Fatalf("explicit name collision retry %+v %v", automatic, err)
	}
	// Entropy exhaustion fails without creating a row or queuing an operation.
	before := countTerminalSessionRows(t, store, machineID)
	if _, err = service.CreateTerminalSession(ctx, userID, machineID, "", "create-no-entropy"); !errors.Is(err, io.EOF) {
		t.Fatalf("entropy failure = %v", err)
	}
	if after := countTerminalSessionRows(t, store, machineID); before != after {
		t.Fatalf("entropy failure created rows: %d -> %d", before, after)
	}
}

func TestAvailabilityPolicyLifecycle(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_availability_" + suffix
	machineID := "um_availability_" + suffix
	environmentID := "env_availability_" + suffix
	helperID := "helper_availability_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "availability-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'linux','amd64','/home/test','online','occupied',true)`, machineID, userID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, environmentID, machineID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, helperID, environmentID); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)

	resolution, err := service.ResolveAvailabilityPolicy(ctx, helperID, environmentID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Schema != AvailabilityPolicySchemaV1 || resolution.UserMachineID != machineID || resolution.Mode != "allow_sleep" || resolution.Version != 0 {
		t.Fatalf("default resolution = %+v", resolution)
	}
	if _, err := service.ResolveAvailabilityPolicy(ctx, "wrong-helper", environmentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong-helper resolution error = %v", err)
	}
	initialObservedAt := time.Now().UTC().Truncate(time.Microsecond)
	initialObservation := AvailabilityObservation{Schema: AvailabilityPolicySchemaV1, Mode: "allow_sleep", Version: 0, Status: "applied", ObservedAt: initialObservedAt, HostServiceVersion: "1.2.3", HostServiceScope: "system", UpdateHealth: "healthy"}
	if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, initialObservation); err != nil {
		t.Fatalf("initial version-zero observation: %v", err)
	}

	first, err := service.SetAvailabilityPolicy(ctx, userID, machineID, "availability-first", "keep_awake", 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.DesiredMode != "keep_awake" || first.DesiredVersion != 1 || first.Status != "pending" {
		t.Fatalf("first policy = %+v", first)
	}
	mismatched := initialObservation
	mismatched.Version = first.DesiredVersion
	mismatched.ObservedAt = initialObservedAt.Add(time.Millisecond)
	if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, mismatched); !errors.Is(err, ErrAvailabilityObservationStale) {
		t.Fatalf("applied observation for a different desired mode: %v", err)
	}
	var observedMode, observedStatus string
	var observedVersion int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT availability_observed_mode,availability_observed_version,availability_status FROM paperboat.user_machines WHERE id=$1`, machineID).Scan(&observedMode, &observedVersion, &observedStatus); err != nil {
		t.Fatal(err)
	}
	if observedMode != initialObservation.Mode || observedVersion != initialObservation.Version || observedStatus != "pending" {
		t.Fatal("rejected availability acknowledgement changed observed state")
	}
	failedObservation := mismatched
	failedObservation.Status, failedObservation.ErrorCode = "error", "keep_awake_failed"
	if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, failedObservation); err != nil {
		t.Fatalf("failed application must report actual old mode: %v", err)
	}
	changedActual := failedObservation
	changedActual.Mode = first.DesiredMode
	changedActual.ObservedAt = failedObservation.ObservedAt.Add(time.Millisecond)
	if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, changedActual); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT availability_observed_mode FROM paperboat.user_machines WHERE id=$1`, machineID).Scan(&observedMode); err != nil || observedMode != changedActual.Mode {
		t.Fatal("changed actual mode was mistaken for an identical failed observation")
	}
	recoveredObservation := failedObservation
	recoveredObservation.Mode, recoveredObservation.Status, recoveredObservation.ErrorCode = first.DesiredMode, "applied", ""
	recoveredObservation.ObservedAt = changedActual.ObservedAt.Add(time.Millisecond)
	if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, recoveredObservation); err != nil {
		t.Fatalf("same desired version cannot recover from failed application: %v", err)
	}
	second, err := service.SetAvailabilityPolicy(ctx, userID, machineID, "availability-second", "allow_sleep", 1)
	if err != nil {
		t.Fatal(err)
	}
	if second.DesiredMode != "allow_sleep" || second.DesiredVersion != 2 {
		t.Fatalf("second policy = %+v", second)
	}
	replayed, err := service.SetAvailabilityPolicy(ctx, userID, machineID, "availability-first", "keep_awake", 0)
	if err != nil {
		t.Fatal(err)
	}
	replayedObservedAt, firstObservedAt := replayed.ObservedAt, first.ObservedAt
	replayed.ObservedAt, first.ObservedAt = nil, nil
	if replayedObservedAt == nil || firstObservedAt == nil || !replayedObservedAt.Equal(*firstObservedAt) || !reflect.DeepEqual(replayed, first) {
		t.Fatalf("durable replay = %+v, want original %+v", replayed, first)
	}
	if _, err := service.SetAvailabilityPolicy(ctx, userID, machineID, "availability-first", "allow_sleep", 0); !errors.Is(err, ErrAvailabilityIdempotencyConflict) {
		t.Fatalf("reused key error = %v", err)
	}
	if _, err := service.SetAvailabilityPolicy(ctx, userID, machineID, "availability-stale", "keep_awake", 1); !errors.Is(err, ErrAvailabilityVersionConflict) {
		t.Fatalf("stale version error = %v", err)
	} else {
		var versionErr *AvailabilityVersionError
		if !errors.As(err, &versionErr) || versionErr.CurrentVersion != 2 {
			t.Fatalf("stale version details = %#v", err)
		}
	}

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	observation := AvailabilityObservation{Schema: AvailabilityPolicySchemaV1, Mode: "allow_sleep", Version: 2, Status: "applied", ObservedAt: observedAt, HostServiceVersion: "1.2.3", HostServiceScope: "system", UpdateRollbacks: 2, UpdateHealth: "recovery_required"}
	if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, observation); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, observation); err != nil {
		t.Fatalf("exact observation replay: %v", err)
	}
	higherRollbacks := observation
	higherRollbacks.ObservedAt = observedAt.Add(time.Second)
	higherRollbacks.UpdateRollbacks = 3
	if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, higherRollbacks); err != nil {
		t.Fatalf("new rollback observation: %v", err)
	}
	if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, observation); !errors.Is(err, ErrAvailabilityObservationStale) {
		t.Fatalf("decreasing rollback observation error = %v", err)
	}
	recovered := higherRollbacks
	recovered.ObservedAt = observedAt.Add(2 * time.Second)
	recovered.Status = "applied"
	recovered.HostServiceVersion = "1.2.4"
	recovered.UpdateHealth = "healthy"
	if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, recovered); err != nil {
		t.Fatalf("same-policy health update: %v", err)
	}
	for name, candidate := range map[string]AvailabilityObservation{
		"stale":      {Schema: AvailabilityPolicySchemaV1, Mode: "keep_awake", Version: 1, Status: "applied", ObservedAt: observedAt, HostServiceVersion: "1.2.3", HostServiceScope: "system", UpdateHealth: "healthy"},
		"future":     {Schema: AvailabilityPolicySchemaV1, Mode: "allow_sleep", Version: 3, Status: "applied", ObservedAt: observedAt, HostServiceVersion: "1.2.3", HostServiceScope: "system", UpdateHealth: "healthy"},
		"same-older": {Schema: AvailabilityPolicySchemaV1, Mode: "allow_sleep", Version: 2, Status: "error", ErrorCode: "apply_failed", ObservedAt: observedAt, HostServiceVersion: "1.2.3", HostServiceScope: "system", UpdateRollbacks: 3, UpdateHealth: "recovery_required"},
	} {
		if err := service.RecordAvailabilityObservation(ctx, environmentID, machineID, candidate); !errors.Is(err, ErrAvailabilityObservationStale) {
			t.Errorf("%s observation error = %v", name, err)
		}
	}

	machine, err := store.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: machineID, UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	availability := mapAvailability(machine)
	if availability.Status != "applied" || availability.ObservedMode != "allow_sleep" || availability.ObservedVersion != 2 || availability.HostServiceVersion != "1.2.4" || availability.UpdateRollbacks != 3 || availability.UpdateHealth != "healthy" {
		t.Fatalf("observed availability = %+v", availability)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET online=false WHERE id=$1`, machineID); err != nil {
		t.Fatal(err)
	}
	machine, err = store.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: machineID, UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	if offline := mapAvailability(machine); offline.Status != "offline" || offline.DesiredVersion != 2 || offline.ObservedVersion != 2 {
		t.Fatalf("offline availability = %+v", offline)
	}
}

func TestReserveBandwidthConsumesIncludedThenTopups(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, userMachineID := "usr_um_bandwidth_"+suffix, "um_bandwidth_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `
INSERT INTO paperboat.users (id, workos_subject, primary_email, status)
VALUES ($1, $2, $3, 'active')`, userID, "workos_"+suffix, "cm-bandwidth-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.SQL().ExecContext(ctx, `
INSERT INTO paperboat.user_machine_entitlements
  (id, user_id, provider_subscription_id, product_code, state, seat_quantity, allowance_bytes, current_period_start, current_period_end)
VALUES ($1, $2, $3, 'connected-test', 'active', 1, 100, $4, $5)`, "ume_"+suffix, userID, "sub_"+suffix, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `
INSERT INTO paperboat.user_machines
  (id, user_id, environment_id, platform, architecture, workspace_root, state, seat_state, online)
VALUES ($1, $2, $3, 'linux', 'amd64', '/home/example', 'online', 'occupied', true)`, userMachineID, userID, "env_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET observed_capabilities=ARRAY['file_receive'] WHERE id=$1`, userMachineID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `
INSERT INTO paperboat.user_machine_bandwidth_topups
  (id, user_id, provider_order_id, purchased_bytes, remaining_bytes)
VALUES ($1, $2, $3, 20, 20)`, "cmbt_"+suffix, userID, "order_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	reservation, err := service.ReserveBandwidth(ctx, userMachineID, 125)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.GrantedBytes != 120 || !reservation.Exhausted {
		t.Fatalf("reservation = %+v, want 120 bytes and exhausted", reservation)
	}
	var included, topup int64
	if err := store.SQL().QueryRowContext(ctx, `
SELECT consumed_included_bytes, consumed_topup_bytes
FROM paperboat.user_machine_bandwidth_periods
WHERE user_machine_id = $1`, userMachineID).Scan(&included, &topup); err != nil {
		t.Fatal(err)
	}
	if included != 100 || topup != 20 {
		t.Fatalf("period consumed included/topup = %d/%d", included, topup)
	}
	var topupState string
	var remaining int64
	if err := store.SQL().QueryRowContext(ctx, `
SELECT state, remaining_bytes FROM paperboat.user_machine_bandwidth_topups WHERE id = $1`, "cmbt_"+suffix).Scan(&topupState, &remaining); err != nil {
		t.Fatal(err)
	}
	if topupState != "exhausted" || remaining != 0 {
		t.Fatalf("top-up state/remaining = %q/%d", topupState, remaining)
	}
	reservation, err = service.ReserveBandwidth(ctx, userMachineID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.GrantedBytes != 0 || !reservation.Exhausted {
		t.Fatalf("exhausted reservation = %+v", reservation)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET state = 'disconnected' WHERE id = $1`, userMachineID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReserveBandwidth(ctx, userMachineID, 1); !errors.Is(err, ErrBandwidthDenied) {
		t.Fatalf("disuser machine reservation error = %v", err)
	}
}

func TestDashboardEnrollmentIsIdempotentSingleClaimAndIssuesMaterial(t *testing.T) {
	integrationPublicIdentityKey := newIntegrationPublicIdentityKey(t)
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_enrollment_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "enrollment-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_enrollment_"+suffix, userID, "sub_enrollment_"+suffix); err != nil {
		t.Fatal(err)
	}
	oldMachineID := "mch_old_enrollment_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES ($1,$2,$3,'studio','darwin','arm64','/Users/paperboat','offline','released')`, oldMachineID, userID, "env_old_enrollment_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"darwin", "linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-enrollment-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureBootstrapCommand("pb machine bootstrap")
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "henr_enrollment_" + suffix, HelperID: "hlp_enrollment_" + suffix, Credential: "enrollment-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	first, err := service.StartEnrollment(ctx, userID, "idem-1")
	if err != nil {
		t.Fatal(err)
	}
	pairingUUID, err := uuid.Parse(strings.TrimPrefix(first.ID, "pairing_"))
	if err != nil || pairingUUID.Version() != 4 || first.ID != "pairing_"+pairingUUID.String() {
		t.Fatalf("invalid pairing identity %q: %v", first.ID, err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET state='pending' WHERE id=$1`, first.ID); err == nil {
		t.Fatal("an unclaimed bootstrap grant authorized an unbound device")
	}
	replayed, err := service.StartEnrollment(ctx, userID, "idem-1")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != first.ID || replayed.BootstrapToken != first.BootstrapToken || replayed.OperationID != first.OperationID {
		t.Fatalf("idempotent replay changed result: first=%+v replay=%+v", first, replayed)
	}
	pairing, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: first.BootstrapToken, Verifier: "verifier-1-" + suffix, Alias: "studio", Platform: "darwin", Architecture: "arm64", WorkspaceRoot: "/Users/paperboat", RuntimeVersions: json.RawMessage(`{"pb":"test"}`), PublicIdentityKey: integrationPublicIdentityKey})
	if err != nil {
		t.Fatal(err)
	}
	if pairing.ID != first.ID {
		t.Fatalf("bootstrap claim changed pairing identity: %q != %q", pairing.ID, first.ID)
	}
	if _, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: first.BootstrapToken, Verifier: "verifier-2-" + suffix, Alias: "replay", Platform: "darwin", Architecture: "arm64", WorkspaceRoot: "/Users/paperboat", PublicIdentityKey: integrationPublicIdentityKey}); !errors.Is(err, ErrEnrollmentTokenUsed) {
		t.Fatalf("consumed bootstrap claim error=%v", err)
	}
	status, err := service.Enrollment(ctx, userID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "material_issued" || status.ID != pairing.ID || status.WorkspaceRoot != "/Users/paperboat" {
		t.Fatalf("claimed status=%+v", status)
	}
	if status.UserMachineID == oldMachineID {
		t.Fatal("fresh identity replaced the old machine")
	}
	var newAlias, oldAlias string
	if err := store.SQL().QueryRowContext(ctx, `SELECT alias FROM paperboat.user_machines WHERE id=$1`, status.UserMachineID).Scan(&newAlias); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT alias FROM paperboat.user_machines WHERE id=$1`, oldMachineID).Scan(&oldAlias); err != nil {
		t.Fatal(err)
	}
	if newAlias != "studio-2" || oldAlias != "studio" {
		t.Fatalf("aliases after fresh enrollment: new=%q old=%q", newAlias, oldAlias)
	}
	// A new one-use command on the same installed identity is a reconnect,
	// not another machine. It must retain the chosen alias and machine ID.
	continued, err := service.StartEnrollment(ctx, userID, "idem-same-machine-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: continued.BootstrapToken, Verifier: "verifier-same-machine-" + suffix, Alias: "studio", Platform: "darwin", Architecture: "arm64", WorkspaceRoot: "/Users/paperboat", RuntimeVersions: json.RawMessage(`{"pb":"test"}`), PublicIdentityKey: integrationPublicIdentityKey}); err != nil {
		t.Fatal(err)
	}
	continuedStatus, err := service.Enrollment(ctx, userID, continued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if continuedStatus.UserMachineID != status.UserMachineID {
		t.Fatalf("same identity created another machine: first=%q continued=%q", status.UserMachineID, continuedStatus.UserMachineID)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT alias FROM paperboat.user_machines WHERE id=$1`, status.UserMachineID).Scan(&newAlias); err != nil {
		t.Fatal(err)
	}
	if newAlias != "studio-2" {
		t.Fatalf("same identity changed its alias to %q", newAlias)
	}
}

// A retired record still holds its name (a fresh enrollment gets the numbered variant) until it
// is removed; once pb reset / start fresh removes it explicitly, the new enrollment takes the exact name.
func TestFreshEnrollmentTakesTheNameOnceTheRetiredRecordIsRemoved(t *testing.T) {
	integrationPublicIdentityKey := newIntegrationPublicIdentityKey(t)
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_retired_alias_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "retired-alias-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_retired_alias_"+suffix, userID, "sub_retired_alias_"+suffix); err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{"disconnected", "offline"} {
		alias := []string{"studio", "desk"}[i]
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES ($1,$2,$3,$4,'darwin','arm64','/Users/paperboat',$5,'released')`, fmt.Sprintf("mch_retired_%d_%s", i, suffix), userID, fmt.Sprintf("env_retired_%d_%s", i, suffix), alias, state); err != nil {
			t.Fatal(err)
		}
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"darwin", "linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-enrollment-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureBootstrapCommand("pb machine bootstrap")
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "henr_retired_" + suffix, HelperID: "hlp_retired_" + suffix, Credential: "enrollment-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	enrollments := 0
	enroll := func(alias, key string) string {
		t.Helper()
		enrollments++
		start, err := service.StartEnrollment(ctx, userID, fmt.Sprintf("idem-%s-%d", alias, enrollments))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: start.BootstrapToken, Verifier: fmt.Sprintf("verifier-%s-%d-%s", alias, enrollments, suffix), Alias: alias, Platform: "darwin", Architecture: "arm64", WorkspaceRoot: "/Users/paperboat", RuntimeVersions: json.RawMessage(`{"pb":"test"}`), PublicIdentityKey: key}); err != nil {
			t.Fatal(err)
		}
		status, err := service.Enrollment(ctx, userID, start.ID)
		if err != nil {
			t.Fatal(err)
		}
		return status.UserMachineID
	}
	aliasOf := func(id string) string {
		t.Helper()
		var alias string
		if err := store.SQL().QueryRowContext(ctx, `SELECT alias FROM paperboat.user_machines WHERE id=$1`, id).Scan(&alias); err != nil {
			t.Fatal(err)
		}
		return alias
	}
	fresh := enroll("studio", integrationPublicIdentityKey)
	if got := aliasOf(fresh); got != "studio-2" {
		t.Fatalf("fresh machine beside a retired record alias=%q, want studio-2", got)
	}
	if got := aliasOf("mch_retired_0_" + suffix); got != "studio" {
		t.Fatalf("retired machine alias=%q, want it untouched", got)
	}
	// pb reset / start fresh removes the old record explicitly, then the computer is added again.
	if err := service.Delete(ctx, userID, "mch_retired_0_"+suffix); err != nil {
		t.Fatal(err)
	}
	again := enroll("studio", newIntegrationPublicIdentityKey(t))
	if got := aliasOf(again); got != "studio" {
		t.Fatalf("machine added after the old record was removed alias=%q, want studio", got)
	}
	live := enroll("desk", newIntegrationPublicIdentityKey(t))
	if got := aliasOf(live); got != "desk-2" {
		t.Fatalf("machine beside a live one alias=%q, want desk-2", got)
	}
	if got := aliasOf("mch_retired_1_" + suffix); got != "desk" {
		t.Fatalf("live machine alias=%q, want desk", got)
	}
}

func TestDashboardEnrollmentsStayBoundToTheirOwnTokenAndInstallation(t *testing.T) {
	integrationPublicIdentityKey := newIntegrationPublicIdentityKey(t)
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_enrollment_isolation_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, userID, "workos_enrollment_isolation_"+suffix, "enrollment-isolation-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',2,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_enrollment_isolation_"+suffix, userID, "sub_enrollment_isolation_"+suffix); err != nil {
		t.Fatal(err)
	}

	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-enrollment-isolation-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureBootstrapCommand("pb machine bootstrap")
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	grantNumber := 0
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		grantNumber++
		return HelperEnrollmentGrant{
			EnrollmentID: fmt.Sprintf("henr_enrollment_isolation_%s_%d", suffix, grantNumber),
			HelperID:     fmt.Sprintf("hlp_enrollment_isolation_%s_%d", suffix, grantNumber),
			Credential:   fmt.Sprintf("enrollment-isolation-credential-%d", grantNumber),
			ExpiresAt:    time.Now().UTC().Add(10 * time.Minute),
		}, nil
	})

	first, err := service.StartEnrollmentWithOptions(ctx, userID, "idem-enrollment-isolation-first-"+suffix, EnrollmentOptions{Shell: "posix"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.StartEnrollmentWithOptions(ctx, userID, "idem-enrollment-isolation-second-"+suffix, EnrollmentOptions{Shell: "posix"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.BootstrapToken == second.BootstrapToken || enrollmentTokenHash(first.BootstrapToken) == enrollmentTokenHash(second.BootstrapToken) {
		t.Fatalf("independent enrollments shared identity: first=%+v second=%+v", first, second)
	}

	tamperedToken := "0" + first.BootstrapToken[1:]
	if first.BootstrapToken[0] == '0' {
		tamperedToken = "2" + first.BootstrapToken[1:]
	}
	if _, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: tamperedToken, Verifier: "verifier-enrollment-isolation-tampered-" + suffix, Alias: "tampered", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace-tampered", PublicIdentityKey: integrationPublicIdentityKey}); !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("metadata-tampered token error = %v, want ErrEnrollmentTokenInvalid", err)
	}

	firstPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	firstPublicKey := base64.RawURLEncoding.EncodeToString(firstPublic)
	firstVerifier := "verifier-enrollment-isolation-first-" + suffix
	firstPairing, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: first.BootstrapToken, Verifier: firstVerifier, Alias: "first-machine", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace-first", PublicIdentityKey: firstPublicKey, RuntimeVersions: json.RawMessage(`{"pb":"test"}`)})
	if err != nil {
		t.Fatal(err)
	}

	secondPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondPublicKey := base64.RawURLEncoding.EncodeToString(secondPublic)
	secondVerifier := "verifier-enrollment-isolation-second-" + suffix
	secondPairing, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: second.BootstrapToken, Verifier: secondVerifier, Alias: "second-machine", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace-second", PublicIdentityKey: secondPublicKey, RuntimeVersions: json.RawMessage(`{"pb":"test"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if firstPairing.ID == secondPairing.ID {
		t.Fatalf("independent tokens shared pairing %q", firstPairing.ID)
	}

	if _, err := service.ConsumeInstallationForIdentity(ctx, firstVerifier, secondPublicKey); !errors.Is(err, ErrInstallationUnavailable) {
		t.Fatalf("cross-identity installation consume error = %v, want ErrInstallationUnavailable", err)
	}
	firstMaterial, err := service.ConsumeInstallationForIdentity(ctx, firstVerifier, firstPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	secondMaterial, err := service.ConsumeInstallationForIdentity(ctx, secondVerifier, secondPublicKey)
	if err != nil {
		t.Fatal(err)
	}

	var firstDocument, secondDocument struct {
		UserMachineID string `json:"user_machine_id"`
		PairingID     string `json:"pairing_id"`
	}
	if err := json.Unmarshal(firstMaterial, &firstDocument); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(secondMaterial, &secondDocument); err != nil {
		t.Fatal(err)
	}
	if firstDocument.PairingID != first.ID || secondDocument.PairingID != second.ID || firstDocument.UserMachineID == secondDocument.UserMachineID {
		t.Fatalf("installation material crossed enrollment boundaries: first=%+v second=%+v", firstDocument, secondDocument)
	}
	firstStatus, err := service.Enrollment(ctx, userID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondStatus, err := service.Enrollment(ctx, userID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstStatus.UserMachineID != firstDocument.UserMachineID || secondStatus.UserMachineID != secondDocument.UserMachineID || firstStatus.UserMachineID == secondStatus.UserMachineID {
		t.Fatalf("enrollment status crossed machine boundaries: first=%+v second=%+v", firstStatus, secondStatus)
	}
}

func TestDashboardEnrollmentApprovalFailureIsRetryable(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_enrollment_approval_failure_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, userID, "workos_approval_failure_"+suffix, "enrollment-approval-failure-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, nil)
	service.ConfigureInstallationEncryption("test-enrollment-key")
	first, err := service.StartEnrollment(ctx, userID, "idem-approval-failure-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreatePairing(ctx, PairingInput{
		EnrollmentToken:   first.BootstrapToken,
		Verifier:          "verifier-approval-failure-" + suffix,
		Alias:             "studio",
		Platform:          "linux",
		Architecture:      "amd64",
		WorkspaceRoot:     "/workspace",
		RuntimeVersions:   json.RawMessage(`{"pb":"test"}`),
		PublicIdentityKey: base64.RawURLEncoding.EncodeToString(public),
	})
	if !errors.Is(err, ErrSeatUnavailable) {
		t.Fatalf("automatic approval error = %v, want ErrSeatUnavailable", err)
	}

	status, err := service.Enrollment(ctx, userID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "failed_retryable" || status.ID != first.ID {
		t.Fatalf("failed enrollment status = %+v, want failed_retryable with pairing", status)
	}
	var pairingState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_pairings WHERE id=$1`, status.ID).Scan(&pairingState); err != nil {
		t.Fatal(err)
	}
	if pairingState != "expired" {
		t.Fatalf("failed enrollment pairing state = %q, want expired", pairingState)
	}

	retried, err := service.RetryEnrollment(ctx, userID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != "awaiting_bootstrap" || retried.Generation != first.Generation+1 || retried.ID != first.ID || retried.BootstrapToken == first.BootstrapToken {
		t.Fatalf("retry = %+v, want fresh awaiting_bootstrap enrollment", retried)
	}
}

func TestEnrollmentDenialAndImmediateApprovalOnPostgres(t *testing.T) {
	integrationPublicIdentityKey := newIntegrationPublicIdentityKey(t)
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_enrollment_deny_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, userID, "workos_deny_"+suffix, "enrollment-deny-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-enrollment-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureBootstrapCommand("pb machine bootstrap")
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "henr_deny_" + suffix, HelperID: "hlp_deny_" + suffix, Credential: "deny-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	// An interactively requested pairing (no dashboard enrollment token) waits for the account owner: denying it
	// is atomic, final and replay-safe, and its installation material is never released.
	verifier := "verifier-deny-" + suffix
	pairing, err := service.CreatePairing(ctx, PairingInput{Verifier: verifier, Alias: "denied-host", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace", PublicIdentityKey: integrationPublicIdentityKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeInstallation(ctx, verifier); !errors.Is(err, ErrInstallationPending) {
		t.Fatalf("pending installation material error = %v", err)
	}
	if err := service.Deny(ctx, userID, pairing.UserCode); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeInstallation(ctx, verifier); !errors.Is(err, ErrInstallationDenied) {
		t.Fatalf("denied installation material error = %v", err)
	}
	var pairingState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_pairings WHERE id=$1`, pairing.ID).Scan(&pairingState); err != nil || pairingState != "denied" {
		t.Fatalf("pairing=%q err=%v", pairingState, err)
	}
	if err := service.Deny(ctx, userID, pairing.UserCode); !errors.Is(err, ErrPairingUsed) {
		t.Fatalf("denial replay error = %v", err)
	}

	// A dashboard-issued enrollment token is already authenticated by the account owner, so claiming it approves the
	// pairing at once (a pasted command is one-shot): there is nothing left to deny.
	first, err := service.StartEnrollment(ctx, userID, "idem-deny-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: first.BootstrapToken, Verifier: "verifier-claimed-" + suffix, Alias: "claimed-host", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace", PublicIdentityKey: newIntegrationPublicIdentityKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	var claimedState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_pairings WHERE id=$1`, claimed.ID).Scan(&claimedState); err != nil || claimedState != "approved" {
		t.Fatalf("a claimed enrollment token left the pairing %q (%v), want approved at once", claimedState, err)
	}
	if err := service.Deny(ctx, userID, claimed.UserCode); !errors.Is(err, ErrPairingUsed) {
		t.Fatalf("denying an already approved pairing = %v, want ErrPairingUsed", err)
	}
}

func TestDashboardEnrollmentExpiryIsAtomicAndRetryable(t *testing.T) {
	integrationPublicIdentityKey := newIntegrationPublicIdentityKey(t)
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_enrollment_expiry_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, userID, "workos_expiry_"+suffix, "enrollment-expiry-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-enrollment-key")
	first, err := service.StartEnrollment(ctx, userID, "idem-expiry-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET expires_at=now()-interval '1 minute' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	_, err = service.CreatePairing(ctx, PairingInput{EnrollmentToken: first.BootstrapToken, Verifier: "verifier-expiry-" + suffix, Alias: "expired-host", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace", PublicIdentityKey: integrationPublicIdentityKey})
	if !errors.Is(err, ErrEnrollmentTokenExpired) {
		t.Fatalf("expired bootstrap claim error=%v", err)
	}
	status, err := service.Enrollment(ctx, userID, first.ID)
	if err != nil || status.ID != first.ID || status.State != "expired" {
		t.Fatalf("expired grant state=%s same_id=%v err=%v", status.State, status.ID == first.ID, err)
	}
	var pairingState, enrollmentState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_pairings WHERE id=$1`, first.ID).Scan(&pairingState); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT installation_state FROM paperboat.user_machine_pairings WHERE id=$1`, first.ID).Scan(&enrollmentState); err != nil {
		t.Fatal(err)
	}
	if pairingState != "expired" || enrollmentState != "expired" {
		t.Fatalf("pairing=%q enrollment=%q", pairingState, enrollmentState)
	}
	var machineCount, occupiedSeats int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE seat_state='occupied') FROM paperboat.user_machines WHERE user_id=$1`, userID).Scan(&machineCount, &occupiedSeats); err != nil {
		t.Fatal(err)
	}
	if machineCount != 0 || occupiedSeats != 0 {
		t.Fatalf("machines=%d occupied_seats=%d", machineCount, occupiedSeats)
	}
	retried, err := service.RetryEnrollment(ctx, userID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Generation != first.Generation+1 || retried.State != "awaiting_bootstrap" || retried.BootstrapToken == first.BootstrapToken || retried.ID != first.ID {
		t.Fatalf("retry=%+v", retried)
	}
}

func TestInstallationMaterialIsSingleUseAndExpiryBound(t *testing.T) {
	integrationPublicIdentityKey := newIntegrationPublicIdentityKey(t)
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_install_replay_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, userID, "workos_install_"+suffix, "install-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_install_replay_"+suffix, userID, "sub_install_replay_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-install-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "henr_install_replay_" + suffix, HelperID: "helper_install_replay_" + suffix, Credential: "install-replay-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	start, err := service.StartEnrollment(ctx, userID, "idem-install-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	verifier := "verifier-install-" + suffix
	pairing, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: start.BootstrapToken, Verifier: verifier, Alias: "install-host", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace", PublicIdentityKey: integrationPublicIdentityKey})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := secrets.Encrypt("test-install-key", `{"bootstrap":"ok"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET state='approved', installation_config_ciphertext=$1 WHERE id=$2`, ciphertext, pairing.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET installation_state='material_issued' WHERE id=$1`, start.ID); err != nil {
		t.Fatal(err)
	}
	material, err := service.ConsumeInstallation(ctx, verifier)
	if err != nil || string(material) != `{"bootstrap":"ok"}` {
		t.Fatalf("material=%s err=%v", material, err)
	}
	replayed, err := service.ConsumeInstallation(ctx, verifier)
	if err != nil || string(replayed) != string(material) {
		t.Fatalf("same-verifier recovery material=%s err=%v", replayed, err)
	}
	if _, err := service.ConsumeInstallation(ctx, verifier+"-wrong"); !errors.Is(err, ErrInstallationUnavailable) {
		t.Fatalf("different-verifier replay error=%v", err)
	}
	var enrollmentState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT installation_state FROM paperboat.user_machine_pairings WHERE id=$1`, start.ID).Scan(&enrollmentState); err != nil {
		t.Fatal(err)
	}
	if enrollmentState != "installing" {
		t.Fatalf("enrollment state=%q", enrollmentState)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET installation_state='ready' WHERE id=$1`, start.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeInstallation(ctx, verifier); !errors.Is(err, ErrInstallationUnavailable) {
		t.Fatalf("completed enrollment replay error=%v", err)
	}

	second, err := service.StartEnrollment(ctx, userID, "idem-install-expired-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	expiredVerifier := "verifier-install-expired-" + suffix
	expiredPairing, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: second.BootstrapToken, Verifier: expiredVerifier, Alias: "expired-host", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace", PublicIdentityKey: integrationPublicIdentityKey})
	if err != nil {
		t.Fatal(err)
	}
	expiredCiphertext, err := secrets.Encrypt("test-install-key", `{"bootstrap":"expired"}`)
	if err != nil {
		t.Fatal(err)
	}
	expiredHash := sha256.Sum256([]byte(expiredVerifier))
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET state='approved', verifier_hash=$1, installation_config_ciphertext=$2, expires_at=now()-interval '1 minute' WHERE id=$3`, expiredHash[:], expiredCiphertext, expiredPairing.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET installation_state='material_issued' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeInstallation(ctx, expiredVerifier); !errors.Is(err, ErrInstallationExpired) {
		t.Fatalf("expired material error=%v", err)
	}
}

func TestExpiredInstallationRecoveryIsCrashSafeConcurrentAndIdentityBound(t *testing.T) {
	t.Run("active_refresh_and_response_loss", func(t *testing.T) { testExpiredInstallationRecovery(t, false) })
	t.Run("explicit_revocation", func(t *testing.T) { testExpiredInstallationRecovery(t, true) })
}

func testExpiredInstallationRecovery(t *testing.T, revoke bool) {
	t.Helper()
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_install_recovery_" + suffix
	publicIdentityDigest := sha256.Sum256([]byte("install-recovery-identity-" + suffix))
	publicIdentityKey := base64.RawURLEncoding.EncodeToString(publicIdentityDigest[:])
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_install_recovery_"+suffix, "install-recovery-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ent_install_recovery_"+suffix, userID, "sub_install_recovery_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-install-recovery-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureCLIAuth("paperboat-cli", []string{"account:read", "machines:connect", "session:refresh"}, 5*time.Minute, 24*time.Hour, "test-cli-hash-key")
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	var grantMu sync.Mutex
	grantCalls := map[string]int{}
	service.ConfigureHelperEnrollment(func(_ context.Context, _ string, operationKey, environmentID string, _ time.Duration) (HelperEnrollmentGrant, error) {
		grantMu.Lock()
		grantCalls[operationKey]++
		grantMu.Unlock()
		kind := "initial"
		if strings.HasPrefix(operationKey, "machine-recovery:") {
			kind = "recovered"
		}
		if kind == "initial" {
			if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active') ON CONFLICT (id) DO NOTHING`, "helper_initial_"+suffix, environmentID); err != nil {
				return HelperEnrollmentGrant{}, err
			}
		}
		return HelperEnrollmentGrant{EnrollmentID: "henr_" + kind + "_" + suffix, HelperID: "helper_" + kind + "_" + suffix, Credential: strings.Repeat(kind, 8), ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	service.ConfigureHelperRecovery(func(_ context.Context, _ string, operationKey, _ string, existingHelperID string, _ time.Duration) (HelperEnrollmentGrant, error) {
		if existingHelperID != "helper_initial_"+suffix {
			return HelperEnrollmentGrant{}, ErrInstallationUnavailable
		}
		grantMu.Lock()
		grantCalls[operationKey]++
		grantMu.Unlock()
		return HelperEnrollmentGrant{EnrollmentID: "henr_recovered_" + suffix, HelperID: "helper_recovered_" + suffix, Credential: strings.Repeat("recovered", 8), ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	start, err := service.StartEnrollmentWithOptions(ctx, userID, "idem-install-recovery-"+suffix, EnrollmentOptions{Shell: "posix"})
	if err != nil {
		t.Fatal(err)
	}
	verifier := "verifier-install-recovery-" + suffix
	pairing, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: start.BootstrapToken, Verifier: verifier, Alias: "recovery-host", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/test", PublicIdentityKey: publicIdentityKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Approve(ctx, userID, pairing.UserCode); err != nil {
		t.Fatal(err)
	}
	originalBody, err := service.ConsumeInstallationForIdentityState(ctx, verifier, publicIdentityKey, false)
	if err != nil {
		t.Fatal(err)
	}
	type recoveryMaterial struct {
		UserMachineID string                    `json:"user_machine_id"`
		PairingID     string                    `json:"pairing_id"`
		EnvironmentID string                    `json:"environment_id"`
		HelperID      string                    `json:"helper_id"`
		ReuseIdentity bool                      `json:"reuse_identity"`
		Generation    int64                     `json:"installation_generation"`
		ClientSession installationClientSession `json:"client_session"`
	}
	var original recoveryMaterial
	if err := json.Unmarshal(originalBody, &original); err != nil {
		t.Fatal(err)
	}
	if original.ClientSession.SessionID == "" || original.ClientSession.RefreshToken == "" || original.ClientSession.AccessToken == "" {
		t.Fatal("original client session is incomplete")
	}
	// A journal that already persisted the runtime identity may recover only
	// against the exact helper bound into its original signed material. This
	// path retains current CLI profile ownership without issuing replacement credentials.
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings
SET expires_at=now()-interval '1 minute', installation_config_consumed_at=now()-interval '1 hour', installation_state='ready'
WHERE id=$1`, pairing.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeInstallationForIdentityState(ctx, verifier, publicIdentityKey, false); !errors.Is(err, ErrInstallationUnavailable) {
		t.Fatalf("ready journal without persisted runtime assertion error=%v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET installation_generation=installation_generation+1 WHERE id=$1`, original.UserMachineID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeInstallationForIdentityState(ctx, verifier, publicIdentityKey, true); !errors.Is(err, ErrInstallationUnavailable) {
		t.Fatal("stale ready-journal generation was accepted")
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET installation_generation=installation_generation-1 WHERE id=$1`, original.UserMachineID); err != nil {
		t.Fatal(err)
	}
	machineAuth := authservice.NewDeviceService(store, audit.NewWriter(store), config.Default().CLIAuth, []string{"test-cli-hash-key"})
	machineAuth.ConfigureAuthorizationEncryptionKey("test-refresh-recovery-encryption")
	currentProfile, err := machineAuth.Refresh(ctx, original.ClientSession.RefreshToken, strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("active daemon initial refresh: %v", err)
	}
	var tokenCountBefore int
	if err := store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.cli_access_tokens WHERE cli_client_session_id=$1) + (SELECT count(*) FROM paperboat.cli_refresh_tokens WHERE cli_client_session_id=$1)`, original.ClientSession.SessionID).Scan(&tokenCountBefore); err != nil {
		t.Fatal(err)
	}
	reusedBody, err := service.ConsumeInstallationForIdentityState(ctx, verifier, publicIdentityKey, true)
	if err != nil {
		t.Fatal(err)
	}
	var reused recoveryMaterial
	if err := json.Unmarshal(reusedBody, &reused); err != nil {
		t.Fatal(err)
	}
	if reused.HelperID != original.HelperID || !reused.ReuseIdentity {
		t.Fatalf("runtime-enrolled recovery changed helper binding: same_helper=%v reuse_identity=%v", original.HelperID == reused.HelperID, reused.ReuseIdentity)
	}
	if reused.ClientSession.SessionID != "" {
		t.Fatal("admitted-runtime recovery must not return stale or replacement CLI credentials")
	}
	var tokenCountAfter int
	if err := store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.cli_access_tokens WHERE cli_client_session_id=$1) + (SELECT count(*) FROM paperboat.cli_refresh_tokens WHERE cli_client_session_id=$1)`, original.ClientSession.SessionID).Scan(&tokenCountAfter); err != nil {
		t.Fatal(err)
	}
	if tokenCountAfter != tokenCountBefore {
		t.Fatal("runtime recovery issued new CLI tokens")
	}
	if _, err := machineAuth.Authenticate(ctx, currentProfile.AccessToken); err != nil {
		t.Fatalf("runtime recovery revoked current access: %v", err)
	}
	// Discard the response as if the network lost it. The existing daemon can
	// refresh, and the same journal can recover again without overwriting it.
	refreshed, err := machineAuth.Refresh(ctx, currentProfile.RefreshToken, strings.Repeat("b", 64))
	if err != nil {
		t.Fatalf("runtime recovery revoked active refresh after lost response: %v", err)
	}
	repeatedRefresh, err := machineAuth.Refresh(ctx, currentProfile.RefreshToken, strings.Repeat("b", 64))
	if err != nil || repeatedRefresh.AccessToken != refreshed.AccessToken || repeatedRefresh.RefreshToken != refreshed.RefreshToken || repeatedRefresh.CLIClientSessionID != refreshed.CLIClientSessionID || repeatedRefresh.ExpiresIn <= 0 || repeatedRefresh.ExpiresIn > refreshed.ExpiresIn {
		t.Fatal("same refresh attempt lost its immutable recovery receipt")
	}
	replayedRuntime, err := service.ConsumeInstallationForIdentityState(ctx, verifier, publicIdentityKey, true)
	if err != nil {
		t.Fatalf("admitted recovery after response loss: %v", err)
	}
	var replayedIdentity recoveryMaterial
	if err := json.Unmarshal(replayedRuntime, &replayedIdentity); err != nil {
		t.Fatal(err)
	}
	if replayedIdentity.UserMachineID != reused.UserMachineID || replayedIdentity.PairingID != reused.PairingID || replayedIdentity.EnvironmentID != reused.EnvironmentID || replayedIdentity.HelperID != reused.HelperID || replayedIdentity.Generation != reused.Generation || !replayedIdentity.ReuseIdentity || replayedIdentity.ClientSession.SessionID != "" {
		t.Fatal("admitted response-loss recovery changed exact identity or returned CLI credentials")
	}
	if _, err := machineAuth.Authenticate(ctx, refreshed.AccessToken); err != nil {
		t.Fatalf("recovery replay revoked refreshed profile: %v", err)
	}
	if revoke {
		if err := machineAuth.RevokeClient(ctx, userID, original.ClientSession.SessionID, "user_logout"); err != nil {
			t.Fatal(err)
		}
		if _, err := service.ConsumeInstallationForIdentityState(ctx, verifier, publicIdentityKey, true); !errors.Is(err, ErrInstallationUnavailable) {
			t.Fatalf("explicitly revoked session recovered runtime: %v", err)
		}
		if _, err := machineAuth.Authenticate(ctx, refreshed.AccessToken); err == nil {
			t.Fatal("explicit revocation retained live profile access")
		}
		return
	}
	reused.ClientSession = original.ClientSession
	reused.ClientSession.AccessToken, reused.ClientSession.RefreshToken = refreshed.AccessToken, refreshed.RefreshToken
	// Recovery preserves the machine/session binding while retiring both old
	// credentials. Verify the durable token records, not just different strings.
	assertRotation := func(previous, next installationClientSession) {
		t.Helper()
		for _, tokens := range []struct{ table, oldToken, newToken string }{
			{"cli_access_tokens", previous.AccessToken, next.AccessToken},
			{"cli_refresh_tokens", previous.RefreshToken, next.RefreshToken},
		} {
			var oldRevoked, newActive bool
			query := "SELECT EXISTS(SELECT 1 FROM paperboat." + tokens.table + " WHERE token_hash=$1 AND cli_client_session_id=$3 AND revoked_at IS NOT NULL), EXISTS(SELECT 1 FROM paperboat." + tokens.table + " WHERE token_hash=$2 AND cli_client_session_id=$3 AND revoked_at IS NULL AND expires_at>now())"
			if err := store.SQL().QueryRowContext(ctx, query, oneShotHash(service.cliHashKey, tokens.oldToken), oneShotHash(service.cliHashKey, tokens.newToken), next.SessionID).Scan(&oldRevoked, &newActive); err != nil {
				t.Fatal(err)
			}
			if !oldRevoked || !newActive {
				t.Fatalf("%s recovery authority: old_revoked=%v new_active=%v", tokens.table, oldRevoked, newActive)
			}
		}
		machineAuth := authservice.NewDeviceService(store, audit.NewWriter(store), config.Default().CLIAuth, []string{"test-cli-hash-key"})
		if _, err := machineAuth.Authenticate(ctx, previous.AccessToken); err == nil {
			t.Fatal("recovery accepted retired access token")
		}
		if principal, err := machineAuth.Authenticate(ctx, next.AccessToken); err != nil || principal.SessionID != next.SessionID {
			t.Fatalf("recovery rejected replacement access token: err=%v", err)
		}
	}
	grantMu.Lock()
	for operation, calls := range grantCalls {
		if strings.HasPrefix(operation, "machine-recovery:") && calls != 0 {
			grantMu.Unlock()
			t.Fatalf("runtime-enrolled recovery issued helper grant %q %d times", operation, calls)
		}
	}
	grantMu.Unlock()
	originalBody, original = reusedBody, reused
	// Simulate a server process dying after BeginUserMachineInstallationRecovery
	// committed but before it issued or stored replacement material. Every retry
	// must reuse this operation key and leave old ciphertext intact until swap.
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings
SET expires_at=now()-interval '1 minute', installation_config_consumed_at=now()-interval '1 hour', installation_recovery_operation_key=NULL, installation_state='installing'
WHERE id=$1`, pairing.ID); err != nil {
		t.Fatal(err)
	}
	storedPairing, err := store.Queries().GetUserMachinePairingByID(ctx, pairing.ID)
	if err != nil {
		t.Fatal(err)
	}
	recoveryOperation := installationRecoveryOperationKey(storedPairing.ID, storedPairing.ExpiresAt)
	begun, err := store.Queries().BeginUserMachineInstallationRecovery(ctx, dbsqlc.BeginUserMachineInstallationRecoveryParams{
		ID: storedPairing.ID, VerifierHash: storedPairing.VerifierHash, PublicIdentityKey: storedPairing.PublicIdentityKey,
		OperationKey: sql.NullString{String: recoveryOperation, Valid: true}, RecoveryAfter: sql.NullTime{Time: time.Now().UTC().Add(-installationRecoveryGrace), Valid: true}, ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	})
	if err != nil || !begun.InstallationRecoveryOperationKey.Valid || begun.InstallationRecoveryOperationKey.String != recoveryOperation {
		t.Fatalf("begin recovery operation bound=%v err=%v", begun.InstallationRecoveryOperationKey.Valid && begun.InstallationRecoveryOperationKey.String == recoveryOperation, err)
	}
	type result struct {
		body json.RawMessage
		err  error
	}
	startGate := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-startGate
			body, recoverErr := service.ConsumeInstallationForIdentityState(ctx, verifier, publicIdentityKey, false)
			results <- result{body: body, err: recoverErr}
		}()
	}
	close(startGate)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent recovery errors: %v / %v", first.err, second.err)
	}
	if !bytes.Equal(first.body, second.body) {
		t.Fatalf("concurrent recovery returned different committed material")
	}
	var recovered recoveryMaterial
	if err := json.Unmarshal(first.body, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.UserMachineID != original.UserMachineID || recovered.PairingID != original.PairingID || recovered.EnvironmentID != original.EnvironmentID {
		t.Fatal("recovery changed machine binding")
	}
	if recovered.HelperID == original.HelperID {
		t.Fatal("runtime-not-enrolled recovery did not rotate the helper enrollment")
	}
	if recovered.ClientSession.SessionID != original.ClientSession.SessionID || recovered.ClientSession.RefreshToken == original.ClientSession.RefreshToken || recovered.ClientSession.AccessToken == original.ClientSession.AccessToken {
		t.Fatalf("recovery client session mismatch: same_session=%v rotated_refresh=%v rotated_access=%v", recovered.ClientSession.SessionID == original.ClientSession.SessionID, recovered.ClientSession.RefreshToken != original.ClientSession.RefreshToken, recovered.ClientSession.AccessToken != original.ClientSession.AccessToken)
	}
	assertRotation(original.ClientSession, recovered.ClientSession)
	grantMu.Lock()
	recoveryGrantCalls := grantCalls[recoveryOperation]
	if recoveryGrantCalls < 1 || recoveryGrantCalls > 2 {
		t.Fatalf("recovery grant calls=%d, want one or two idempotent calls", recoveryGrantCalls)
	}
	for operation, calls := range grantCalls {
		if strings.HasPrefix(operation, "machine-recovery:") && operation != recoveryOperation && calls != 0 {
			t.Fatalf("unexpected recovery operation %q called %d times", operation, calls)
		}
	}
	grantMu.Unlock()
	var operation sql.NullString
	var activeAccessTokens, revokedAccessTokens int
	if err := store.SQL().QueryRowContext(ctx, `SELECT installation_recovery_operation_key FROM paperboat.user_machine_pairings WHERE id=$1`, pairing.ID).Scan(&operation); err != nil {
		t.Fatal(err)
	}
	if operation.Valid {
		t.Fatalf("recovery operation was not cleared: %q", operation.String)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE revoked_at IS NULL),count(*) FILTER (WHERE revoked_at IS NOT NULL) FROM paperboat.cli_access_tokens WHERE cli_client_session_id=$1`, original.ClientSession.SessionID).Scan(&activeAccessTokens, &revokedAccessTokens); err != nil {
		t.Fatal(err)
	}
	if activeAccessTokens != 1 || revokedAccessTokens < 1 {
		t.Fatalf("access token states active=%d revoked=%d", activeAccessTokens, revokedAccessTokens)
	}
	replayed, err := service.ConsumeInstallationForIdentityState(ctx, verifier, publicIdentityKey, false)
	if err != nil || !bytes.Equal(replayed, first.body) {
		t.Fatalf("recovery replay changed material: same_material=%v err=%v", bytes.Equal(replayed, first.body), err)
	}
	if _, err := service.ConsumeInstallationForIdentityState(ctx, verifier, publicIdentityKey+"wrong", false); !errors.Is(err, ErrInstallationUnavailable) {
		t.Fatalf("wrong identity recovery error=%v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET expires_at=now()-interval '1 minute',installation_config_consumed_at=now()-interval '25 hours' WHERE id=$1`, pairing.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeInstallationForIdentityState(ctx, verifier, publicIdentityKey, false); !errors.Is(err, ErrInstallationExpired) {
		t.Fatalf("recovery beyond fixed grace error=%v", err)
	}
}

func TestInstallationFailureRevokesIdentityReleasesSeatAndRetryIssuesNewIdentity(t *testing.T) {
	integrationPublicIdentityKey := newIntegrationPublicIdentityKey(t)
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, userMachineID, environmentID := "usr_install_failure_"+suffix, "um_install_failure_"+suffix, "env_install_failure_"+suffix
	pairingID, helperID := newID("pairing"), "helper_install_failure_"+suffix
	enrollmentID := pairingID
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_install_failure_"+suffix, "install-failure-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,public_identity_key) VALUES ($1,$2,$3,'recovery-host','linux','amd64','/workspace','offline','occupied',$4)`, userMachineID, userID, environmentID, integrationPublicIdentityKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_pairings (id,verifier_hash,user_code,requested_alias,platform,architecture,workspace_root,public_identity_key,state,user_machine_id,expires_at,installation_state,generation) VALUES ($1,$2,$3,'recovery-host','linux','amd64','/workspace',$4,'consumed',$5,now()+interval '10 minutes','installing',1)`, pairingID, []byte("verifier_"+suffix), "CODE"+suffix, integrationPublicIdentityKey, userMachineID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_pairings SET bootstrap_owner_account=$2,operation_id=$3,idempotency_key=$4,bootstrap_token_hash=$5,bootstrap_token_ciphertext=$6,installation_state='installing' WHERE id=$1 AND user_machine_id=$7`, enrollmentID, userID, "op_"+suffix, "idem_"+suffix, []byte("token_"+suffix), []byte("cipher_"+suffix), userMachineID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, environmentID, userMachineID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,target_host,target_port,desired_state,desired_revision,applied_revision) VALUES ($1,$2,'runtime_https_wss',$3,'127.0.0.1',38080,'attached',1,0)`, "route_"+suffix, environmentID, "recovery-"+suffix+".example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, helperID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helper_enrollments (id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,state,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,'consumed',now()+interval '10 minutes')`, "henr_"+suffix, environmentID, helperID, []byte("jti_"+suffix), "op_helper_"+suffix, []byte("request_"+suffix), []byte("grant_"+suffix)); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-install-key")
	service.ConfigureAccess(nil, "https://control.example.test")
	if err := service.ConfigureRuntimeRoute("example.test", 38080); err != nil {
		t.Fatal(err)
	}
	grantCalls := 0
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		grantCalls++
		newHelperID, newEnrollmentID := "helper_install_retry_"+suffix, "henr_retry_"+suffix
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'pending')`, newHelperID, environmentID); err != nil {
			return HelperEnrollmentGrant{}, err
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helper_enrollments (id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,state,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',now()+interval '10 minutes')`, newEnrollmentID, environmentID, newHelperID, []byte("jti_retry_"+suffix), "op_helper_retry_"+suffix, []byte("request_retry_"+suffix), []byte("grant_retry_"+suffix)); err != nil {
			return HelperEnrollmentGrant{}, err
		}
		return HelperEnrollmentGrant{EnrollmentID: newEnrollmentID, HelperID: newHelperID, Credential: "retry-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	if err := service.FailInstallation(ctx, enrollmentID, environmentID, "helper_wrong", "henr_"+suffix, "service_install"); !errors.Is(err, ErrEnrollmentState) {
		t.Fatalf("cross-helper failure report error = %v", err)
	}
	if err := service.FailInstallation(ctx, enrollmentID, environmentID, helperID, "henr_wrong_"+suffix, "service_install"); !errors.Is(err, ErrEnrollmentState) {
		t.Fatalf("mismatched helper-enrollment failure report error = %v", err)
	}
	if err := service.FailInstallation(ctx, enrollmentID, environmentID, helperID, "", "service_install"); !errors.Is(err, ErrEnrollmentState) {
		t.Fatalf("unbound failure report error = %v", err)
	}
	if err := service.FailInstallation(ctx, enrollmentID, environmentID, helperID, "henr_"+suffix, "service_install"); err != nil {
		t.Fatal(err)
	}
	var failedEnrollmentState, failedMachineSeat, failedHelperState, failedGrantState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT e.installation_state,m.seat_state,h.state,he.state FROM paperboat.user_machine_pairings e JOIN paperboat.user_machines m ON m.id=e.user_machine_id JOIN paperboat.control_helpers h ON h.environment_id=m.environment_id JOIN paperboat.control_helper_enrollments he ON he.helper_id=h.id WHERE e.id=$1 AND h.id=$2 AND he.id=$3`, enrollmentID, helperID, "henr_"+suffix).Scan(&failedEnrollmentState, &failedMachineSeat, &failedHelperState, &failedGrantState); err != nil {
		t.Fatal(err)
	}
	if failedEnrollmentState != "failed_retryable" || failedMachineSeat != "released" || failedHelperState != "revoked" || failedGrantState != "revoked" {
		t.Fatalf("failure cleanup enrollment=%s seat=%s helper=%s grant=%s", failedEnrollmentState, failedMachineSeat, failedHelperState, failedGrantState)
	}
	retried, err := service.RetryEnrollment(ctx, userID, enrollmentID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Generation != 2 || retried.State != "awaiting_bootstrap" || retried.UserMachineID != userMachineID || retried.ID != enrollmentID {
		t.Fatalf("retry state=%s generation=%d same_machine=%v same_pairing=%v", retried.State, retried.Generation, retried.UserMachineID == userMachineID, retried.ID == enrollmentID)
	}
	retryVerifier := "retry-verifier-" + suffix
	retryPairing, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: retried.BootstrapToken, Verifier: retryVerifier, Alias: "recovery-host", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace", PublicIdentityKey: integrationPublicIdentityKey})
	if err != nil {
		t.Fatal(err)
	}
	recoveredMachine, err := service.Approve(ctx, userID, retryPairing.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredMachine.ID != userMachineID || recoveredMachine.EnvironmentID != environmentID || grantCalls != 1 {
		t.Fatalf("machine=%+v helper_grant_calls=%d", recoveredMachine, grantCalls)
	}
	material, err := service.ConsumeInstallation(ctx, retryVerifier)
	if err != nil {
		t.Fatal(err)
	}
	var recoveryMaterial struct {
		UserMachineID       string `json:"user_machine_id"`
		PairingID           string `json:"pairing_id"`
		EnvironmentID       string `json:"environment_id"`
		HelperID            string `json:"helper_id"`
		ReuseIdentity       bool   `json:"reuse_identity"`
		Credential          string `json:"enrollment_credential"`
		HelperListenAddress string `json:"helper_listen_address"`
	}
	if json.Unmarshal(material, &recoveryMaterial) != nil || recoveryMaterial.UserMachineID != userMachineID || recoveryMaterial.PairingID != enrollmentID || recoveryMaterial.EnvironmentID != environmentID || recoveryMaterial.HelperID != "helper_install_retry_"+suffix || recoveryMaterial.ReuseIdentity || recoveryMaterial.Credential != "retry-credential" || recoveryMaterial.HelperListenAddress != "127.0.0.1:38080" {
		t.Fatal("recovery material binding mismatch")
	}
	var routeCount int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.control_routes WHERE environment_id=$1 AND kind='runtime_https_wss' AND public_host=$2 AND target_host='127.0.0.1' AND target_port=38080`, environmentID, "recovery-"+suffix+".example.test").Scan(&routeCount); err != nil || routeCount != 1 {
		t.Fatalf("runtime route count=%d err=%v", routeCount, err)
	}
	var machineCount, occupiedSeats int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE seat_state='occupied') FROM paperboat.user_machines WHERE user_id=$1`, userID).Scan(&machineCount, &occupiedSeats); err != nil {
		t.Fatal(err)
	}
	if machineCount != 1 || occupiedSeats != 1 {
		t.Fatalf("machines=%d occupied_seats=%d", machineCount, occupiedSeats)
	}
}

func TestMachineControlInitialReplayRotatesAfterExpiryWithoutCrossMachineCollision(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_machine_control_" + suffix
	machineID := "mch_machine_control_" + suffix
	otherMachineID := "mch_machine_control_other_" + suffix
	environmentID := "env_machine_control_" + suffix
	otherEnvironmentID := "env_machine_control_other_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_machine_control_"+suffix, "machine-control-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	insertMachine := func(id, env string, key ed25519.PublicKey) {
		t.Helper()
		_, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,configured_capabilities,observed_capabilities,public_identity_key,installation_generation) VALUES ($1,$2,$3,'windows','amd64','C:\workspace','offline','occupied',ARRAY['file_receive','preview_launch']::text[],ARRAY[]::text[],$4,3)`, id, userID, env, base64.RawURLEncoding.EncodeToString(key))
		if err != nil {
			t.Fatal(err)
		}
	}
	insertMachine(machineID, environmentID, publicKey)
	insertMachine(otherMachineID, otherEnvironmentID, otherPublicKey)
	machine, err := store.Queries().GetActiveUserMachineForControl(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureMachineControl(signer, "https://api.example.test")
	now := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	service.now = func() time.Time { return now }
	operationID := "machine-control-initial-" + suffix
	first, err := service.mintMachineControl(ctx, machine, operationID)
	if err != nil {
		t.Fatal(err)
	}
	firstClaims, err := signer.VerifyCredential(first.Credential, "https://api.example.test", "machine_control", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if firstClaims.MachineID != machineID || firstClaims.EnvironmentID != environmentID || firstClaims.UserID != userID || firstClaims.InstallationGeneration != 3 || firstClaims.KeyThumbprint != machineKeyThumbprint(machine) {
		t.Fatalf("first claims=%+v", firstClaims)
	}

	now = now.Add(machineControlTTL + time.Minute)
	second, err := service.mintMachineControl(ctx, machine, operationID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Credential == first.Credential || !second.ExpiresAt.After(now) {
		t.Fatalf("expired replay did not rotate: first=%+v second=%+v", first, second)
	}
	secondClaims, err := signer.VerifyCredential(second.Credential, "https://api.example.test", "machine_control", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if secondClaims.JTI == firstClaims.JTI || secondClaims.MachineID != machineID || secondClaims.EnvironmentID != environmentID || secondClaims.UserID != userID || secondClaims.InstallationGeneration != 3 || secondClaims.KeyThumbprint != machineKeyThumbprint(machine) {
		t.Fatalf("rotated claims=%+v first=%+v", secondClaims, firstClaims)
	}

	now = now.Add(time.Minute)
	replay, err := service.mintMachineControl(ctx, machine, operationID)
	if err != nil || replay.Credential == "" {
		t.Fatalf("valid replay credential=%+v err=%v", replay, err)
	}
	replayClaims, err := signer.VerifyCredential(replay.Credential, "https://api.example.test", "machine_control", now)
	if err != nil || replayClaims.JTI != secondClaims.JTI || !replay.ExpiresAt.Equal(second.ExpiresAt) {
		t.Fatalf("valid replay claims=%+v credential=%+v err=%v", replayClaims, replay, err)
	}
	if replayClaims.SessionGeneration != secondClaims.SessionGeneration || replayClaims.SessionGeneration <= firstClaims.SessionGeneration {
		t.Fatalf("session generations first=%d second=%d replay=%d", firstClaims.SessionGeneration, secondClaims.SessionGeneration, replayClaims.SessionGeneration)
	}

	newOperationID := "machine-control-reconnect-" + suffix
	reconnected, err := service.mintMachineControl(ctx, machine, newOperationID)
	if err != nil {
		t.Fatal(err)
	}
	reconnectedClaims, err := signer.VerifyCredential(reconnected.Credential, "https://api.example.test", "machine_control", now)
	if err != nil || reconnectedClaims.SessionGeneration != replayClaims.SessionGeneration+1 {
		t.Fatalf("reconnected claims=%+v err=%v", reconnectedClaims, err)
	}
	if _, err := store.Queries().GetCurrentMachineControlSession(ctx, dbsqlc.GetCurrentMachineControlSessionParams{MachineID: machineID, InstallationGeneration: 3, CredentialJti: replayClaims.JTI, Now: now}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("superseded credential lookup err=%v, want sql.ErrNoRows", err)
	}
	current, err := store.Queries().GetCurrentMachineControlSession(ctx, dbsqlc.GetCurrentMachineControlSessionParams{MachineID: machineID, InstallationGeneration: 3, CredentialJti: reconnectedClaims.JTI, Now: now})
	if err != nil || current.SessionGeneration != reconnectedClaims.SessionGeneration {
		t.Fatalf("current session=%+v err=%v", current, err)
	}
	if _, err := service.mintMachineControl(ctx, machine, operationID); !errors.Is(err, ErrMachineControlInvalid) {
		t.Fatalf("superseded operation replay err=%v, want ErrMachineControlInvalid", err)
	}

	otherMachine, err := store.Queries().GetActiveUserMachineForControl(ctx, otherMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.mintMachineControl(ctx, otherMachine, operationID); !errors.Is(err, ErrMachineControlInvalid) {
		t.Fatalf("cross-machine operation reuse error=%v, want ErrMachineControlInvalid", err)
	}
	var storedMachine, storedJTI string
	if err := store.SQL().QueryRowContext(ctx, `SELECT machine_id,credential_jti FROM paperboat.machine_control_renewals WHERE operation_id=$1`, operationID).Scan(&storedMachine, &storedJTI); err != nil {
		t.Fatal(err)
	}
	if storedMachine != machineID || storedJTI != secondClaims.JTI {
		t.Fatalf("cross-machine attempt changed reservation machine=%q jti=%q", storedMachine, storedJTI)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET seat_state='released' WHERE id=$1`, machineID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.IssueInitialMachineControl(ctx, machineID, environmentID, "released-control-"+suffix); !errors.Is(err, ErrMachineControlInvalid) {
		t.Fatalf("released machine initial control grant: %v", err)
	}
	if _, err := store.Queries().GetOwnedActiveUserMachineForControl(ctx, dbsqlc.GetOwnedActiveUserMachineForControlParams{ID: machineID, UserID: userID}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("released machine owned control authority: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET seat_state='occupied' WHERE id=$1`, machineID); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{}`)
	bodyDigest := sha256.Sum256(body)
	claims := MachineProofClaims{MachineID: machineID, EnvironmentID: environmentID, InstallationGeneration: 3, OperationID: "renew-before-remove-" + suffix, Method: "POST", Path: "/v1/machine-control-renewals", BodySHA256: base64.RawURLEncoding.EncodeToString(bodyDigest[:]), IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := json.Marshal(machineProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))})
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := service.RenewMachineControl(ctx, reconnected.Credential, proof, body, "POST", claims.Path)
	if err != nil {
		t.Fatal("live machine-control credential rejected", err)
	}
	// Prove the exact credential/proof pair used below succeeds while the
	// machine is active, so revocation is the only changed authorization input.
	if _, err := service.RenewMachineControl(ctx, renewed.Credential, proof, body, "POST", claims.Path); err != nil {
		t.Fatal("current machine-control credential/proof baseline", err)
	}
	// Clients since b684f426 (F21-13) put a one-use nonce in every proof. Renewal decoded proofs strictly
	// and refused the unknown field, so every renewal on prod answered 401 (1,248 of 1,261 in 6 h) and
	// machines went offline until they re-issued. A nonce-bearing proof renews (replay refusal: TestMachineProofNonceIsUsedOnceOnPostgres).
	nonceClaims := claims
	nonceClaims.OperationID, nonceClaims.Nonce = "renew-nonce-"+suffix, "n0nce-"+strings.Repeat("a", 20)
	noncePayload, err := json.Marshal(nonceClaims)
	if err != nil {
		t.Fatal(err)
	}
	nonceProof, err := json.Marshal(machineProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(noncePayload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, noncePayload))})
	if err != nil {
		t.Fatal(err)
	}
	renewed, err = service.RenewMachineControl(ctx, renewed.Credential, nonceProof, body, "POST", claims.Path)
	if err != nil {
		t.Fatal("renewal with a nonce-bearing proof rejected", err)
	}
	// F21-02: a correctly signed proof from a clock 10 minutes ahead is a clock_skew refusal, not a bad
	// credential, so the machine corrects its time instead of being treated as signed out.
	skewClaims := claims
	skewClaims.OperationID, skewClaims.IssuedAt, skewClaims.ExpiresAt = "renew-skew-"+suffix, now.Add(10*time.Minute), now.Add(11*time.Minute)
	skewPayload, err := json.Marshal(skewClaims)
	if err != nil {
		t.Fatal(err)
	}
	skewProof, err := json.Marshal(machineProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(skewPayload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, skewPayload))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RenewMachineControl(ctx, renewed.Credential, skewProof, body, "POST", claims.Path); ControlRejectionReason(err) != ProofClockSkewReason {
		t.Fatalf("skewed proof = %v, want reason %s", err, ProofClockSkewReason)
	}
	if err := service.Delete(ctx, userID, machineID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RenewMachineControl(ctx, renewed.Credential, proof, body, "POST", claims.Path); !errors.Is(err, ErrMachineControlInvalid) {
		t.Fatalf("removed machine-control credential accepted: %v", err)
	}
	for table := range map[string]struct{}{
		"machine_control_sessions": {},
		"machine_control_renewals": {},
	} {
		var count int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.`+table+` WHERE machine_id=$1`, machineID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows=%d, want 0 after delete", table, count)
		}
	}
	if err := service.Delete(ctx, userID, machineID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete replay error=%v, want ErrNotFound", err)
	}
}

func TestInitialMachineControlPreservesDatabaseFailure(t *testing.T) {
	store := testStore(t)
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, nil, Policy{}, nil)
	service.ConfigureMachineControl(signer, "https://api.example.test")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = service.IssueInitialMachineControl(context.Background(), "machine_1", "environment_1", "initial-machine-control-test")
	if err == nil || errors.Is(err, ErrMachineControlInvalid) {
		t.Fatalf("initial machine-control error=%v; want underlying database failure rather than invalid proof", err)
	}
}

func TestConnectIssuesEnvironmentBoundDescriptor(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, userMachineID, environmentID := "usr_um_connect_"+suffix, "um_connect_"+suffix, "env_connect_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "cm-connect-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'darwin','arm64','/Users/paperboat','online','occupied',true)`, userMachineID, userID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET observed_capabilities=configured_capabilities WHERE id=$1`, userMachineID); err != nil {
		t.Fatal(err)
	}
	sourceMachineID := "um_source_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'darwin','arm64','/Users/source','online','occupied',true)`, sourceMachineID, userID, "env_source_"+suffix); err != nil {
		t.Fatal(err)
	}
	publicIdentityKey := newIntegrationPublicIdentityKey(t)
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET public_identity_key=$2,worker_generation=1 WHERE id=$1`, userMachineID, publicIdentityKey); err != nil {
		t.Fatal(err)
	}
	helperID := "helper_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, environmentID, userMachineID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, helperID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helper_enrollments (id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,state,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',now()+interval '10 minutes')`, "enroll_"+suffix, environmentID, helperID, []byte("jti_"+suffix), "operation_"+suffix, []byte("request_"+suffix), []byte("grant_"+suffix)); err != nil {
		t.Fatal(err)
	}
	edgeNodeID := "edge_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES ($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain',100,now())`, edgeNodeID, "epoch_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_connector_generations (environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES ($1,$2,1,'development',$3,'admitted')`, environmentID, userMachineID, edgeNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES ($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080,1,1,$4,1)`, "route_"+suffix, environmentID, "machine-"+suffix+".example.test", edgeNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.runtime_carrier_attachments
(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active)
VALUES ($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,$9,'{}',now()+interval '1 hour',true,now(),true)`, "route_"+suffix, userMachineID, userID, "process_"+suffix, edgeNodeID, "epoch_"+suffix, publicIdentityKey, "session_"+uuid.NewString(), "session_"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	terminalSessionID := "umts_connect_" + suffix
	if _, err := store.Queries().CreateUserMachineTerminalSession(ctx, dbsqlc.CreateUserMachineTerminalSessionParams{OwnerAccount: userID, ID: terminalSessionID, UserMachineID: userMachineID, Name: "bright-beacon", LaunchCwd: "/Users/paperboat"}); err != nil {
		t.Fatal(err)
	}
	staleSessionID := "umts_stale_" + suffix
	if _, err := store.Queries().CreateUserMachineTerminalSession(ctx, dbsqlc.CreateUserMachineTerminalSessionParams{OwnerAccount: userID, ID: staleSessionID, UserMachineID: userMachineID, Name: "stale", LaunchCwd: "/Users/paperboat"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Queries().QueueUserMachineTerminalSessionOperation(ctx, dbsqlc.QueueUserMachineTerminalSessionOperationParams{ID: "umtso_stale_" + suffix, UserMachineID: userMachineID, TerminalSessionID: staleSessionID, Operation: "delete_history"}); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureInstallationEncryption("test-key")
	service.ConfigureAccess(fakeCredentialIssuer{}, "https://api.paperboat.test")
	signer, err := mint.NewEphemeral(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureTerminalSessions(signer, nil)
	service.ConfigureTransferModes(2_000_000_000, "2026.10.27.0")
	response, err := service.ConnectTerminalSession(ctx, userID, sourceMachineID, userMachineID, "cls_1", terminalSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !response.Connectable || response.UserMachineID != userMachineID || response.Environment["id"] != environmentID || response.Environment["resource_id"] != userMachineID {
		t.Fatalf("response = %#v", response)
	}
	terminalEndpoints, _ := response.Terminal["endpoints"].(map[string]any)
	if terminalEndpoints["wss"] != "wss://machine-"+suffix+".example.test/v1/runtime" || terminalEndpoints["quic"] != "quic://machine-"+suffix+".example.test:443" || response.FileTransfer["endpoint"] != "https://machine-"+suffix+".example.test/v1/file-transfers" || response.Terminal["auth"] == nil || response.FileTransfer["auth"] == nil {
		t.Fatalf("descriptor = %#v", response)
	}
	terminalAuth := response.Terminal["auth"].(map[string]any)
	transferAuth := response.FileTransfer["auth"].(map[string]any)
	accessSessionID, _ := terminalAuth["access_session_id"].(string)
	if accessSessionID == "" || transferAuth["access_session_id"] != accessSessionID {
		t.Fatalf("application credentials do not share access session: terminal=%#v transfer=%#v", terminalAuth, transferAuth)
	}
	for class, token := range map[string]string{"terminal_operation": terminalAuth["token"].(string), "file_transfer": transferAuth["token"].(string)} {
		claims, verifyErr := signer.VerifyCredential(token, "https://api.paperboat.test", class, time.Now().UTC())
		if verifyErr != nil {
			t.Fatalf("verify %s credential: %v", class, verifyErr)
		}
		if class == "terminal_operation" && (claims.WorkspaceID != "personal" || claims.ActorAccountID != userID) {
			t.Fatal("terminal grant lost its verified process context")
		}
		if claims.EnvironmentID != environmentID || claims.UserID != userID || claims.CLIClientSessionID != "cls_1" || claims.SessionID != terminalSessionID || claims.AssignmentID != accessSessionID || class == "file_transfer" && claims.SourceMachineID != sourceMachineID {
			t.Fatalf("%s credential bindings = %#v", class, claims)
		}
	}
}

func TestCreateAndConnectTerminalSessionCompositionIsIdempotent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, userMachineID, environmentID := "usr_um_mc_"+suffix, "um_mc_"+suffix, "env_mc_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "mc-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	publicIdentityKey := newIntegrationPublicIdentityKey(t)
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,worker_generation) VALUES ($1,$2,$3,'darwin','arm64','/Users/paperboat','online','occupied',true,$4,1)`, userMachineID, userID, environmentID, publicIdentityKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET observed_capabilities=configured_capabilities WHERE id=$1`, userMachineID); err != nil {
		t.Fatal(err)
	}
	sourceMachineID := "um_mc_source_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'darwin','arm64','/Users/source','online','occupied',true)`, sourceMachineID, userID, "env_mc_source_"+suffix); err != nil {
		t.Fatal(err)
	}
	helperID := "helper_mc_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, environmentID, userMachineID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, helperID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helper_enrollments (id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,state,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',now()+interval '10 minutes')`, "enroll_mc_"+suffix, environmentID, helperID, []byte("jti_mc_"+suffix), "operation_mc_"+suffix, []byte("request_mc_"+suffix), []byte("grant_mc_"+suffix)); err != nil {
		t.Fatal(err)
	}
	edgeNodeID := "edge_mc_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES ($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain',100,now())`, edgeNodeID, "epoch_mc_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_connector_generations (environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES ($1,$2,1,'development',$3,'admitted')`, environmentID, userMachineID, edgeNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES ($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080,1,1,$4,1)`, "route_mc_"+suffix, environmentID, "machine-mc-"+suffix+".example.test", edgeNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.runtime_carrier_attachments
(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active)
VALUES ($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,$9,'{}',now()+interval '1 hour',true,now(),true)`, "route_mc_"+suffix, userMachineID, userID, "process_mc_"+suffix, edgeNodeID, "epoch_mc_"+suffix, publicIdentityKey, "session_"+uuid.NewString(), "session_"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	var carrierReady bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.runtime_carrier_current($1,true)`, "route_mc_"+suffix).Scan(&carrierReady); err != nil || !carrierReady {
		t.Fatalf("runtime carrier readiness = %v, %v", carrierReady, err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureInstallationEncryption("test-key")
	service.ConfigureAccess(fakeCredentialIssuer{}, "https://api.paperboat.test")
	signer, err := mint.NewEphemeral(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureTerminalSessions(signer, nil)
	service.ConfigureTransferModes(2_000_000_000, "2026.10.27.0")

	created, err := service.CreateConfiguredTerminalSessionInDirectory(ctx, userID, userMachineID, userMachineID, "bench-run", "pb-mc-key", "/tmp/local project")
	if err != nil {
		t.Fatal(err)
	}
	var persistedCWD string
	if err := store.SQL().QueryRowContext(ctx, `SELECT launch_cwd FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, created.ID).Scan(&persistedCWD); err != nil || persistedCWD != "/tmp/local project" {
		t.Fatalf("persisted launch directory = %q, %v", persistedCWD, err)
	}
	if _, err := service.CreateConfiguredTerminalSessionInDirectory(ctx, userID, sourceMachineID, userMachineID, "other-source", "other-source-key", "/tmp/other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-machine launch directory = %v", err)
	}
	if _, err := service.CreateConfiguredTerminalSessionInDirectory(ctx, "unauthorized-user", userMachineID, userMachineID, "unauthorized", "unauthorized-key", "/tmp/other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized launch directory = %v", err)
	}
	if _, err := service.CreateConfiguredTerminalSessionInDirectory(ctx, userID, userMachineID, userMachineID, "invalid-cwd", "invalid-cwd-key", "relative"); !errors.Is(err, ErrTerminalSessionInvalidCWD) {
		t.Fatalf("invalid launch directory = %v", err)
	}
	terminalUUID, err := uuid.Parse(strings.TrimPrefix(created.ID, "terminal_"))
	if err != nil || terminalUUID.Version() != 4 || terminalUUID.Variant() != uuid.RFC4122 || created.ID != "terminal_"+terminalUUID.String() {
		t.Fatalf("invalid canonical terminal identity %q: %v", created.ID, err)
	}
	runtime := &terminalCompositionRuntime{signer: signer, machineID: userMachineID, environmentID: environmentID, userID: userID, sessionID: created.ID}
	service.controlRuntime = runtime
	pending, err := service.ConnectTerminalSession(ctx, userID, userMachineID, userMachineID, "cls_mc_1", created.ID)
	if err != nil || pending.Connectable || pending.Reason != "terminal_session_operation_pending" {
		t.Fatalf("pending operation connectable=%v reason=%s err=%v", pending.Connectable, pending.Reason, err)
	}
	runtime.ready = true
	descriptor, err := service.ConnectTerminalSession(ctx, userID, userMachineID, userMachineID, "cls_mc_1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConnectTerminalSession(ctx, "unauthorized-user", userMachineID, userMachineID, "cls_mc_1", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized self connection = %v", err)
	}
	if !descriptor.Connectable || descriptor.Terminal["session_id"] != created.ID || descriptor.Terminal["cwd"] != "/tmp/local project" {
		t.Fatalf("descriptor connectable=%v status=%s reason=%s session=%v cwd=%v", descriptor.Connectable, descriptor.Status, descriptor.Reason, descriptor.Terminal["session_id"], descriptor.Terminal["cwd"])
	}
	var applied bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT o.state='applied' AND o.operation='create' AND s.runtime_state='running' FROM paperboat.user_machine_terminal_session_operations o JOIN paperboat.user_machine_terminal_sessions s ON s.id=o.terminal_session_id WHERE o.id=$1 AND s.id=$2`, runtime.operationID, created.ID).Scan(&applied); err != nil || !applied {
		t.Fatalf("durable terminal create acknowledgment = %v, %v", applied, err)
	}
	replay, err := service.CreateConfiguredTerminalSessionInDirectory(ctx, userID, userMachineID, userMachineID, "bench-run", "pb-mc-key", "/tmp/changed")
	if err != nil || replay.ID != created.ID || replay.StartedIn != "/tmp/local project" {
		t.Fatalf("idempotent replay = %+v, %v", replay, err)
	}
}

func TestExecDescriptorPersistsExactRevocableCredential(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID, environmentID := "usr_exec_"+suffix, "um_exec_"+suffix, "env_exec_"+suffix
	sourceMachineID, helperID, edgeNodeID := "um_exec_source_"+suffix, "helper_exec_"+suffix, "edge_exec_"+suffix
	publicIdentityKey := newIntegrationPublicIdentityKey(t)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, []any{userID, "workos_exec_" + suffix, "exec-" + suffix + "@example.test"}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,worker_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,$4,1)`, []any{machineID, userID, environmentID, publicIdentityKey}},
		{`UPDATE paperboat.user_machines SET observed_capabilities=configured_capabilities WHERE id=$1`, []any{machineID}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'linux','amd64','/source','online','occupied',true)`, []any{sourceMachineID, userID, "env_exec_source_" + suffix}},
		{`INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, []any{environmentID, machineID, userID}},
		{`INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, []any{helperID, environmentID}},
		{`INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES ($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain',100,now())`, []any{edgeNodeID, "epoch_exec_" + suffix}},
		{`INSERT INTO paperboat.control_connector_generations (environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES ($1,$2,1,'development',$3,'admitted')`, []any{environmentID, machineID, edgeNodeID}},
		{`INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES ($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080,1,1,$4,1)`, []any{"route_exec_" + suffix, environmentID, "exec-" + suffix + ".example.test", edgeNodeID}},
		{`INSERT INTO paperboat.runtime_carrier_attachments
(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active)
VALUES ($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,$9,'{}',now()+interval '1 hour',true,now(),true)`, []any{"route_exec_" + suffix, machineID, userID, "process_exec_" + suffix, edgeNodeID, "epoch_exec_" + suffix, publicIdentityKey, "session_" + uuid.NewString(), "session_" + uuid.NewString()}},
	}
	for _, statement := range statements {
		if _, err := store.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	var carrierReady bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.runtime_carrier_current($1,true)`, "route_exec_"+suffix).Scan(&carrierReady); err != nil || !carrierReady {
		t.Fatalf("exec fixture runtime carrier readiness=%v err=%v", carrierReady, err)
	}
	signer, err := mint.NewEphemeral(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureAccess(fakeCredentialIssuer{}, "https://api.paperboat.test")
	service.ConfigureTerminalSessions(signer, nil)
	operationID := "operation_exec_" + suffix
	descriptor, err := service.ExecDescriptor(ctx, userID, sourceMachineID, machineID, "cli_exec_1", operationID, false)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := descriptor.Auth["token"].(string)
	claims, err := signer.VerifyCredential(token, "https://api.paperboat.test", "exec_operation", time.Now().UTC())
	if err != nil || claims.OperationID != operationID || claims.MachineID != machineID || claims.UserID != userID || claims.CLIClientSessionID != "cli_exec_1" || claims.WorkspaceID != "personal" || claims.ActorAccountID != userID {
		t.Fatalf("claims=%#v err=%v", claims, err)
	}
	var sessionID, state string
	var accessSessionID string
	if err := store.SQL().QueryRowContext(ctx, `SELECT id,helper_terminal_session_id,state FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1`, machineID).Scan(&accessSessionID, &sessionID, &state); err != nil || sessionID != claims.JTI || state != "active" || claims.AssignmentID != accessSessionID || descriptor.Auth["access_session_id"] != accessSessionID {
		t.Fatalf("session id=%q state=%q err=%v", sessionID, state, err)
	}
	// A local client and machine runtime remain distinct authenticated endpoints
	// even when their enrolled machine ID is the same.
	selfOperationID := "operation_exec_self_" + suffix
	selfDescriptor, err := service.ExecDescriptor(ctx, userID, machineID, machineID, "cli_exec_1", selfOperationID, false)
	if err != nil {
		t.Fatalf("owned self exec descriptor: %v", err)
	}
	selfToken, _ := selfDescriptor.Auth["token"].(string)
	selfClaims, err := signer.VerifyCredential(selfToken, "https://api.paperboat.test", "exec_operation", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if selfClaims.SourceMachineID != machineID || selfClaims.MachineID != machineID || selfClaims.OperationID != selfOperationID || selfClaims.WorkspaceID != "personal" || selfClaims.ActorAccountID != userID || selfClaims.UserID != userID || selfClaims.CLIClientSessionID != "cli_exec_1" || selfClaims.AssignmentID == claims.AssignmentID || selfClaims.JTI == claims.JTI {
		t.Fatalf("self exec bindings: source=%t target=%t operation=%t workspace=%t actor=%t user=%t client=%t fresh_access=%t fresh_token=%t",
			selfClaims.SourceMachineID == machineID, selfClaims.MachineID == machineID, selfClaims.OperationID == selfOperationID,
			selfClaims.WorkspaceID == "personal", selfClaims.ActorAccountID == userID, selfClaims.UserID == userID,
			selfClaims.CLIClientSessionID == "cli_exec_1", selfClaims.AssignmentID != claims.AssignmentID, selfClaims.JTI != claims.JTI)
	}
	selfAccessID, _ := selfDescriptor.Auth["access_session_id"].(string)
	if selfAccessID == "" || selfAccessID != selfClaims.AssignmentID {
		t.Fatal("self exec descriptor does not identify its exact revocable authority")
	}
	if _, err := service.ExecDescriptor(ctx, "usr_unrelated_"+suffix, machineID, machineID, "cli_exec_1", "operation_exec_foreign_"+suffix, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("self IDs bypassed source ownership: %v", err)
	}
	if err := service.RevokeUserMachineSessions(ctx, machineID, "test_revocation"); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_access_sessions WHERE id=$1`, accessSessionID).Scan(&state); err != nil || state != "revoked" {
		t.Fatalf("revoked state=%q err=%v", state, err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_access_sessions WHERE id=$1`, selfAccessID).Scan(&state); err != nil || state != "revoked" {
		t.Fatalf("self credential remained authorized after session revocation: state=%q err=%v", state, err)
	}
	if err := service.Disconnect(ctx, userID, machineID); err != nil {
		t.Fatal(err)
	}
	blocked, err := service.ExecDescriptor(ctx, userID, machineID, machineID, "cli_exec_1", "operation_exec_revoked_"+suffix, false)
	if !errors.Is(err, ErrMachineCapabilityUnavailable) || blocked.Auth != nil || blocked.OperationID != "" {
		t.Fatalf("disconnected owned machine must return capability unavailable without issuing authority: err=%v credential=%t operation=%t", err, blocked.Auth != nil, blocked.OperationID != "")
	}
}

func TestTransferDefaultsAndBrokerPreserveMachineOwnershipAndRouteHost(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_transfer_" + suffix
	foreignUserID := "usr_transfer_foreign_" + suffix
	for _, user := range []struct{ id, email string }{{userID, "transfer-" + suffix + "@example.test"}, {foreignUserID, "transfer-foreign-" + suffix + "@example.test"}} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, user.id, "workos_"+user.id, user.email); err != nil {
			t.Fatal(err)
		}
	}
	type machineFixture struct{ id, environmentID, ownerID, name string }
	source := machineFixture{"um_transfer_source_" + suffix, "env_transfer_source_" + suffix, userID, "Source"}
	destination := machineFixture{"um_transfer_destination_" + suffix, "env_transfer_destination_" + suffix, userID, "Destination"}
	host := machineFixture{"um_transfer_host_" + suffix, "env_transfer_host_" + suffix, userID, "Host"}
	foreign := machineFixture{"um_transfer_foreign_" + suffix, "env_transfer_foreign_" + suffix, foreignUserID, "Foreign"}
	for _, machine := range []machineFixture{source, destination, host, foreign} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'linux','amd64','/home/test','online','occupied',true)`, machine.id, machine.ownerID, machine.environmentID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET observed_capabilities=configured_capabilities WHERE id=$1`, machine.id); err != nil {
			t.Fatal(err)
		}
	}
	addRoute := func(machine machineFixture, publicHost string) {
		helperID := "helper_" + machine.id
		edgeID := "edge_" + machine.id
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, machine.environmentID, machine.id, machine.ownerID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, helperID, machine.environmentID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES ($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain-'||$1,100,now())`, edgeID, "epoch_"+machine.id); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_connector_generations (environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES ($1,$2,1,'development',$3,'admitted')`, machine.environmentID, machine.id, edgeID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,managed_public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES ($1,$2,'runtime_https_wss',$3,$3,'127.0.0.1',8080,1,1,$4,1)`, "route_"+machine.id, machine.environmentID, publicHost, edgeID); err != nil {
			t.Fatal(err)
		}
		identityKey := newIntegrationPublicIdentityKey(t)
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET public_identity_key=$2,worker_generation=1 WHERE id=$1`, machine.id, identityKey); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.runtime_carrier_attachments
(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active)
VALUES ($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,$9,'{}',now()+interval '1 hour',true,now(),true)`, "route_"+machine.id, machine.id, machine.ownerID, "process_"+machine.id, edgeID, "epoch_"+machine.id, identityKey, "session_"+uuid.NewString(), "session_"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	addRoute(destination, "destination-"+suffix+".example.test")
	addRoute(host, "host-"+suffix+".example.test")
	sessionID := "umts_transfer_" + suffix
	if _, err := store.Queries().CreateUserMachineTerminalSession(ctx, dbsqlc.CreateUserMachineTerminalSessionParams{OwnerAccount: userID, ID: sessionID, UserMachineID: host.id, Name: "transfer", LaunchCwd: "/home/test"}); err != nil {
		t.Fatal(err)
	}

	signer, err := mint.NewEphemeral(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	policy := accessdescriptor.FileTransferPolicy{Revision: "file-transfer-v1", MaxFileBytes: 50 << 20, MaxBatchFiles: 10, MaxBatchBytes: 500 << 20, MaxConcurrentTransfers: 2, RetentionSeconds: 604800, DeliveryTimeoutSeconds: 600, MaxPendingSpoolBytes: 1 << 30}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureTerminalSessions(signer, nil)
	service.ConfigureFileTransfer(policy)

	if _, err := service.SetTransferDestinationDefault(ctx, userID, foreign.id); !errors.Is(err, ErrTransferDestinationInvalid) {
		t.Fatalf("foreign default error = %v", err)
	}
	if selected, err := service.SetTransferDestinationDefault(ctx, userID, destination.id); err != nil || selected.ID != destination.id {
		t.Fatalf("set user default = %+v, %v", selected, err)
	}
	if selected, err := service.TransferDestinationDefault(ctx, userID); err != nil || selected.ID != destination.id {
		t.Fatalf("get user default = %+v, %v", selected, err)
	}
	if selected, err := service.SetTerminalSessionTransferDestination(ctx, userID, sessionID, destination.id); err != nil || selected.ID != destination.id {
		t.Fatalf("set session default = %+v, %v", selected, err)
	}
	if selected, err := service.TerminalSessionTransferDestination(ctx, userID, sessionID); err != nil || selected.ID != destination.id {
		t.Fatalf("get session default = %+v, %v", selected, err)
	}

	direct, err := service.FileTransferDescriptor(ctx, userID, source.id, destination.id, "cli_1", "", TransferIntent{})
	if err != nil {
		t.Fatal(err)
	}
	if direct.Endpoint != "https://destination-"+suffix+".example.test/v1/file-transfers" || direct.DestinationMachineID != destination.id || direct.Policy != policy {
		t.Fatalf("direct descriptor = %+v", direct)
	}
	session, err := service.FileTransferDescriptor(ctx, userID, source.id, destination.id, "cli_1", sessionID, TransferIntent{})
	if err != nil {
		t.Fatal(err)
	}
	if session.Endpoint != "https://host-"+suffix+".example.test/v1/file-transfers" || session.DestinationMachineID != destination.id {
		t.Fatalf("session descriptor = %+v", session)
	}
	token, _ := session.Auth["token"].(string)
	claims, err := signer.VerifyCredential(token, "https://api.paperboat.test", "file_transfer", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if claims.MachineID != host.id || claims.SourceMachineID != source.id || claims.SessionID != sessionID || claims.UserID != userID || claims.AssignmentID == "" || session.Auth["access_session_id"] != claims.AssignmentID {
		t.Fatalf("session credential bindings = %+v", claims)
	}
	if _, err := service.FileTransferDescriptor(ctx, userID, foreign.id, destination.id, "cli_1", "", TransferIntent{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign source error = %v", err)
	}

	if err := service.ClearTerminalSessionTransferDestination(ctx, userID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.TerminalSessionTransferDestination(ctx, userID, sessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared session default error = %v", err)
	}
	if err := service.ClearTransferDestinationDefault(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.TransferDestinationDefault(ctx, userID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared user default error = %v", err)
	}
}

func TestDisconnectRevokesMintedHelperSessionsAndRetriesOfflineConnector(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, userMachineID, environmentID := "usr_um_revoke_"+suffix, "um_revoke_"+suffix, "env_revoke_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "cm-revoke-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	publicIdentityKey := newIntegrationPublicIdentityKey(t)
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,provider_route_route_id,provider_route_client_id,provider_route_http_base_url,provider_route_websocket_base_url,public_identity_key,worker_generation) VALUES ($1,$2,$3,'darwin','arm64','/Users/paperboat','online','occupied',true,'tun_1','cli_1','https://machine.example','wss://machine.example',$4,1)`, userMachineID, userID, environmentID, publicIdentityKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET observed_capabilities=configured_capabilities WHERE id=$1`, userMachineID); err != nil {
		t.Fatal(err)
	}
	sourceMachineID := "um_source_revoke_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'darwin','arm64','/Users/source','online','occupied',true)`, sourceMachineID, userID, "env_source_revoke_"+suffix); err != nil {
		t.Fatal(err)
	}
	helperID := "helper_revoke_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, environmentID, userMachineID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, helperID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helper_enrollments (id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,state,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',now()+interval '10 minutes')`, "enroll_revoke_"+suffix, environmentID, helperID, []byte("jti_revoke_"+suffix), "operation_revoke_"+suffix, []byte("request_revoke_"+suffix), []byte("grant_revoke_"+suffix)); err != nil {
		t.Fatal(err)
	}
	edgeNodeID := "edge_revoke_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES ($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain',100,now())`, edgeNodeID, "epoch_revoke_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_connector_generations (environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES ($1,$2,1,'development',$3,'admitted')`, environmentID, userMachineID, edgeNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation,desired_state) VALUES ($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080,1,1,$4,1,'attached')`, "route_revoke_"+suffix, environmentID, "machine-revoke-"+suffix+".example.test", edgeNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.runtime_carrier_attachments
(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at)
VALUES ($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,'session_00000000-0000-4000-8000-000000000001','{}',now()+interval '1 hour',true,now())`, "route_revoke_"+suffix, userMachineID, userID, "process_revoke_"+suffix, edgeNodeID, "epoch_revoke_"+suffix, publicIdentityKey, "session_00000000-0000-4000-8000-000000000002"); err != nil {
		t.Fatal(err)
	}
	terminalSessionID := "umts_revoke_" + suffix
	if _, err := store.Queries().CreateUserMachineTerminalSession(ctx, dbsqlc.CreateUserMachineTerminalSessionParams{OwnerAccount: userID, ID: terminalSessionID, UserMachineID: userMachineID, Name: "bright-beacon", LaunchCwd: "/Users/paperboat"}); err != nil {
		t.Fatal(err)
	}
	issuer := &recordingIssuer{}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureInstallationEncryption("test-key")
	service.ConfigureAccess(issuer, "https://api.paperboat.test")
	if _, err := service.ConnectTerminalSession(ctx, userID, sourceMachineID, userMachineID, "cls_1", terminalSessionID); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1 AND state='active'`, userMachineID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active access sessions = %d, want 1", active)
	}
	issuer.failRevocation = true
	if err := service.Disconnect(ctx, userID, userMachineID); err == nil {
		t.Fatal("Disconnect succeeded while Helper was unavailable")
	}
	var environmentState, helperState, enrollmentState, connectorState, routeState, machineState, seatState string
	var online bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT e.desired_state,h.state,he.state,c.state,r.desired_state,m.state,m.seat_state,m.online
FROM paperboat.control_environments e
JOIN paperboat.control_helpers h ON h.environment_id=e.id
JOIN paperboat.control_helper_enrollments he ON he.environment_id=e.id
JOIN paperboat.control_connector_generations c ON c.environment_id=e.id
JOIN paperboat.control_routes r ON r.environment_id=e.id
JOIN paperboat.user_machines m ON m.environment_id=e.id
WHERE e.id=$1`, environmentID).Scan(&environmentState, &helperState, &enrollmentState, &connectorState, &routeState, &machineState, &seatState, &online); err != nil {
		t.Fatal(err)
	}
	if environmentState != "revoked" || helperState != "revoked" || enrollmentState != "revoked" || connectorState != "revoked" || routeState != "detaching" || machineState != "disconnected" || seatState != "released" || online {
		t.Fatalf("disconnect convergence: environment=%s helper=%s enrollment=%s connector=%s route=%s machine=%s seat=%s online=%v", environmentState, helperState, enrollmentState, connectorState, routeState, machineState, seatState, online)
	}
	var propagated bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT helper_revoked_at IS NOT NULL FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1`, userMachineID).Scan(&propagated); err != nil {
		t.Fatal(err)
	}
	if propagated {
		t.Fatal("revocation was marked propagated after a failed downstream call")
	}
	issuer.failRevocation = false
	if err := service.RetryPendingRevocations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT helper_revoked_at IS NOT NULL FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1`, userMachineID).Scan(&propagated); err != nil {
		t.Fatal(err)
	}
	fixtureRevocations := 0
	for _, revocation := range issuer.revocations {
		if revocation.EnvironmentID == environmentID {
			fixtureRevocations++
		}
	}
	if !propagated || fixtureRevocations != 2 {
		t.Fatalf("propagated=%v fixture revocations=%d, want true and 2", propagated, fixtureRevocations)
	}
}

func TestEntitlementLossRevokesMachineControlPlaneWithoutActiveSessions(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, userMachineID, environmentID, helperID := "usr_um_seat_"+suffix, "um_seat_"+suffix, "env_seat_"+suffix, "helper_seat_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "cm-seat-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,revoked_at) VALUES ($1,$2,$3,'linux','amd64','/srv/workspace','revoked','released',false,now())`, userMachineID, userID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, environmentID, userMachineID, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, helperID, environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helper_enrollments (id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,state,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',now()+interval '10 minutes')`, "enroll_seat_"+suffix, environmentID, helperID, []byte("jti_seat_"+suffix), "operation_seat_"+suffix, []byte("request_seat_"+suffix), []byte("grant_seat_"+suffix)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_connector_generations (environment_id,machine_id,generation,edge_pool,state) VALUES ($1,$2,1,'development','admitted')`, environmentID, helperID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,target_host,target_port) VALUES ($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080)`, "route_seat_"+suffix, environmentID, "seat-"+suffix+".example.test"); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	if err := service.ReconcileUserMachineEntitlement(ctx, userID); err != nil {
		t.Fatal(err)
	}
	var environmentState, helperState, enrollmentState, connectorState, routeState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT e.desired_state,h.state,he.state,c.state,r.desired_state
FROM paperboat.control_environments e
JOIN paperboat.control_helpers h ON h.environment_id=e.id
JOIN paperboat.control_helper_enrollments he ON he.environment_id=e.id
JOIN paperboat.control_connector_generations c ON c.environment_id=e.id
JOIN paperboat.control_routes r ON r.environment_id=e.id
WHERE e.id=$1`, environmentID).Scan(&environmentState, &helperState, &enrollmentState, &connectorState, &routeState); err != nil {
		t.Fatal(err)
	}
	if environmentState != "revoked" || helperState != "revoked" || enrollmentState != "revoked" || connectorState != "revoked" || routeState != "detached" {
		t.Fatalf("seat-loss convergence: environment=%s helper=%s enrollment=%s connector=%s route=%s", environmentState, helperState, enrollmentState, connectorState, routeState)
	}
}

func TestSeatReductionRevokesNewestExcessMachine(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_um_seat_reduction_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "cm-seat-reduction-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements
  (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end)
VALUES ($1,$2,$3,'user-machine-seat','active',1,1048576,now()-interval '1 day',now()+interval '1 month')`, "ent_seat_reduction_"+suffix, userID, "sub_seat_reduction_"+suffix); err != nil {
		t.Fatal(err)
	}

	type machineFixture struct {
		id, environmentID, helperID, label string
		enrolledOffset                     string
	}
	machines := []machineFixture{
		{"um_old_" + suffix, "env_old_" + suffix, "helper_old_" + suffix, "Old", "2 days"},
		{"um_new_" + suffix, "env_new_" + suffix, "helper_new_" + suffix, "New", "1 day"},
	}
	for _, machine := range machines {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines
  (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,enrolled_at,created_at)
VALUES ($1,$2,$3,'linux','amd64','/srv/workspace','online','occupied',true,now()-$4::interval,now()-$4::interval)`, machine.id, userID, machine.environmentID, machine.enrolledOffset); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, machine.environmentID, machine.id, userID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, machine.helperID, machine.environmentID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_connector_generations (environment_id,machine_id,generation,edge_pool,state) VALUES ($1,$2,1,'development','admitted')`, machine.environmentID, machine.helperID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,target_host,target_port) VALUES ($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080)`, "route_"+machine.id, machine.environmentID, machine.id+".example.test"); err != nil {
			t.Fatal(err)
		}
	}

	service := New(store, audit.NewWriter(store), Policy{}, nil)
	if err := service.ReconcileUserMachineEntitlement(ctx, userID); err != nil {
		t.Fatal(err)
	}
	for index, machine := range machines {
		var machineState, seatState, environmentState, helperState, connectorState, routeState string
		var online bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT m.state,m.seat_state,m.online,e.desired_state,h.state,c.state,r.desired_state
FROM paperboat.user_machines m
JOIN paperboat.control_environments e ON e.id=m.environment_id
JOIN paperboat.control_helpers h ON h.environment_id=e.id
JOIN paperboat.control_connector_generations c ON c.environment_id=e.id
JOIN paperboat.control_routes r ON r.environment_id=e.id
WHERE m.id=$1`, machine.id).Scan(&machineState, &seatState, &online, &environmentState, &helperState, &connectorState, &routeState); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			if machineState != "online" || seatState != "occupied" || !online || environmentState != "active" || helperState != "active" || connectorState != "admitted" || routeState != "attached" {
				t.Fatalf("kept machine states = %s/%s/%v/%s/%s/%s/%s", machineState, seatState, online, environmentState, helperState, connectorState, routeState)
			}
			continue
		}
		if machineState != "revoked" || seatState != "released" || online || environmentState != "revoked" || helperState != "revoked" || connectorState != "revoked" || routeState != "detached" {
			t.Fatalf("excess machine states = %s/%s/%v/%s/%s/%s/%s", machineState, seatState, online, environmentState, helperState, connectorState, routeState)
		}
	}
}

type recordingIssuer struct {
	fakeCredentialIssuer
	failRevocation bool
	revocations    []access.CredentialRevocationInput
}

type fakeCredentialIssuer struct{}

func (fakeCredentialIssuer) CheckCLI(context.Context, access.CredentialInput) error { return nil }
func (fakeCredentialIssuer) IssueCLI(_ context.Context, input access.CredentialInput) (access.CLICredentials, error) {
	return access.CLICredentials{
		TerminalAuth:      map[string]any{"method": "bearer", "token": "test-terminal", "expires_at": input.ExpiresAt, "scopes": []string{"terminal:operate"}},
		FileTransferAuth:  map[string]any{"method": "bearer", "token": "test-transfer", "expires_at": input.ExpiresAt, "scopes": []string{"file:transfer"}},
		TerminalSessionID: "test-terminal-session", FileSessionID: "test-file-session",
	}, nil
}
func (fakeCredentialIssuer) RevokeCLI(context.Context, access.CredentialRevocationInput) error {
	return nil
}

func (i *recordingIssuer) RevokeCLI(_ context.Context, input access.CredentialRevocationInput) error {
	i.revocations = append(i.revocations, input)
	if i.failRevocation {
		return errors.New("helper unavailable")
	}
	return nil
}

func fatalWithPostgresLocks(t *testing.T, store *db.DB, format string, args ...any) {
	t.Helper()
	message := fmt.Sprintf(format, args...)
	t.Fatalf("%s\nPostgreSQL lock state:\n%s", message, postgresLockDiagnostics(store))
}

func postgresLockDiagnostics(store *db.DB) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := store.SQL().QueryContext(ctx, `
SELECT activity.pid,
       activity.state,
       coalesce(activity.wait_event_type, ''),
       coalesce(activity.wait_event, ''),
       pg_blocking_pids(activity.pid)::text,
       coalesce(lock.locktype, ''),
       coalesce(lock.mode, ''),
       coalesce(lock.granted, false),
       coalesce(lock.relation::regclass::text, ''),
       coalesce(lock.page::text, ''),
       coalesce(lock.tuple::text, ''),
       left(regexp_replace(activity.query, E'[\\n\\r]+', ' ', 'g'), 240)
FROM pg_stat_activity AS activity
LEFT JOIN pg_locks AS lock ON lock.pid = activity.pid
WHERE activity.datname = current_database()
  AND activity.pid <> pg_backend_pid()
  AND activity.state <> 'idle'
ORDER BY activity.pid, lock.granted, lock.locktype, lock.mode
LIMIT 100`)
	if err != nil {
		return "diagnostic query failed: " + err.Error()
	}
	defer rows.Close()
	var result strings.Builder
	for rows.Next() {
		var pid int
		var state, waitType, waitEvent, blockers, lockType, lockMode, relation, page, tuple, query string
		var granted bool
		if err := rows.Scan(&pid, &state, &waitType, &waitEvent, &blockers, &lockType, &lockMode, &granted, &relation, &page, &tuple, &query); err != nil {
			return "diagnostic scan failed: " + err.Error()
		}
		fmt.Fprintf(&result, "pid=%d state=%s wait=%s/%s blockers=%s lock=%s/%s granted=%t relation=%s page=%s tuple=%s query=%q\n", pid, state, waitType, waitEvent, blockers, lockType, lockMode, granted, relation, page, tuple, query)
	}
	if err := rows.Err(); err != nil {
		return "diagnostic rows failed: " + err.Error()
	}
	if result.Len() == 0 {
		return "no non-idle PostgreSQL sessions"
	}
	return result.String()
}

func waitForUserMachineRowLock(ctx context.Context, store *db.DB, machineID string) error {
	return waitForPostgresRowLock(ctx, store, `SELECT id FROM paperboat.user_machines WHERE id=$1 FOR UPDATE NOWAIT`, machineID)
}

func waitForCLIClientSessionRowLock(ctx context.Context, store *db.DB, sessionID string) error {
	return waitForPostgresRowLock(ctx, store, `SELECT id FROM paperboat.cli_client_sessions WHERE id=$1 FOR UPDATE NOWAIT`, sessionID)
}

func waitForPostgresRowLock(ctx context.Context, store *db.DB, query, id string) error {
	for {
		var lockedID string
		err := store.SQL().QueryRowContext(ctx, query, id).Scan(&lockedID)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return nil
		}
		if err != nil {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func testStore(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run user-machine integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return store
}

// Model administrator invalidation during a pending installation while retaining
// the enrolled machine. This exercises exact generation fencing without a machine role.
func invalidateTestInstallation(ctx context.Context, service *Service, userID, machineID string) (UserMachine, error) {
	var out UserMachine
	err := service.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		row, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: machineID, UserID: userID})
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE user_machines SET installation_generation=installation_generation+1,online=false,state='offline',observed_capabilities='{}' WHERE id=$1`, machineID); err != nil {
			return err
		}
		if err = expireAuthenticatedMachineInstallationPairingsTx(ctx, tx, machineID, service.now().UTC()); err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlHelperEnrollmentsForEnvironment(ctx, dbsqlc.RevokeControlHelperEnrollmentsForEnvironmentParams{EnvironmentID: row.EnvironmentID, RevokedAt: sql.NullTime{Time: service.now().UTC(), Valid: true}}); err != nil {
			return err
		}
		row, err = tx.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: machineID, UserID: userID})
		if err != nil {
			return err
		}
		out = mapMachine(row)
		return nil
	})
	return out, err
}

// The fixture runtime acknowledges the actual signed durable create operation.
// Its first admission is unavailable so the test also preserves the pending gate.
type terminalCompositionRuntime struct {
	helperruntime.Client
	signer                                                   *mint.Provider
	machineID, environmentID, userID, sessionID, operationID string
	ready                                                    bool
}

func (r *terminalCompositionRuntime) CreateTerminal(_ context.Context, route, credential, sessionID, operationID, name, cwd string) (helperruntime.Snapshot, error) {
	claims, err := r.signer.VerifyCredential(credential, "https://api.paperboat.test", "terminal_operation", time.Now().UTC())
	if err != nil || route == "" || claims.MachineID != r.machineID || claims.EnvironmentID != r.environmentID || claims.UserID != r.userID || claims.SessionID != r.sessionID || claims.CLIClientSessionID != operationID || sessionID != r.sessionID || operationID == "" || name != "bench-run" || cwd != "/tmp/local project" {
		return helperruntime.Snapshot{}, fmt.Errorf("terminal create binding mismatch: %w", err)
	}
	if r.operationID != "" && r.operationID != operationID {
		return helperruntime.Snapshot{}, errors.New("terminal create retry changed operation identity")
	}
	r.operationID = operationID
	if !r.ready {
		return helperruntime.Snapshot{}, errors.New("fixture terminal host not ready")
	}
	return helperruntime.Snapshot{ID: sessionID, Name: name, CWD: cwd, State: "running", Generation: 1}, nil
}

func oneShotHash(key []byte, value string) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil))
}

func TestMachineControlRenewalReplayAfterLostResponse(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID, otherID := "usr_mc_replay_"+suffix, "mch_mc_replay_"+suffix, "mch_mc_replay_other_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_mc_replay_"+suffix, "mc-replay-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PrivateKey{}
	for _, id := range []string{machineID, otherID} {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		keys[id] = private
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,configured_capabilities,observed_capabilities,public_identity_key,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/work','offline','occupied',ARRAY['file_receive']::text[],ARRAY[]::text[],$4,3)`, id, userID, "env_"+id, base64.RawURLEncoding.EncodeToString(public)); err != nil {
			t.Fatal(err)
		}
	}
	machine, err := store.Queries().GetActiveUserMachineForControl(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureMachineControl(signer, "https://api.example.test")
	now := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	service.now = func() time.Time { return now }
	body := []byte(`{}`)
	renew := func(credential, machine, operation string) (MachineControlCredential, error) {
		t.Helper()
		digest := sha256.Sum256(body)
		claims := MachineProofClaims{MachineID: machine, EnvironmentID: "env_" + machine, InstallationGeneration: 3, OperationID: operation, Method: "POST", Path: "/v1/machine-control-renewals", BodySHA256: base64.RawURLEncoding.EncodeToString(digest[:]), IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
		payload, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := json.Marshal(machineProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(keys[machine], payload))})
		if err != nil {
			t.Fatal(err)
		}
		return service.RenewMachineControl(ctx, credential, proof, body, "POST", claims.Path)
	}
	initial, err := service.mintMachineControl(ctx, machine, "mc-initial-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	operation := "mc-renew-" + suffix
	committed, err := renew(initial.Credential, machineID, operation)
	if err != nil {
		t.Fatal(err)
	}
	// The response was lost: the client retries with the credential it still holds.
	now = now.Add(time.Second)
	replayed, err := renew(initial.Credential, machineID, operation)
	if err != nil || replayed.Credential != committed.Credential || !replayed.ExpiresAt.Equal(committed.ExpiresAt) {
		t.Fatalf("lost-response replay = %+v err=%v, want the committed credential %+v", replayed, err, committed)
	}
	if _, err := renew(initial.Credential, machineID, "mc-other-"+suffix); !errors.Is(err, ErrMachineControlInvalid) {
		t.Fatalf("rotated credential with a new operation err=%v, want ErrMachineControlInvalid", err)
	}
	if _, err := renew(initial.Credential, otherID, operation); !errors.Is(err, ErrMachineControlInvalid) {
		t.Fatalf("another machine's proof replayed the renewal: %v", err)
	}
	// Once the committed credential renews, the older operation is no longer replayable.
	if _, err := renew(committed.Credential, machineID, "mc-next-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := renew(initial.Credential, machineID, operation); !errors.Is(err, ErrMachineControlInvalid) {
		t.Fatalf("replay after a newer renewal err=%v, want ErrMachineControlInvalid", err)
	}
}

// At the plan's machine limit Add machine is refused before any install command is issued, with
// the typed limit error the console and CLI name (machines: Free 100, here configured to 1).
func TestEnrollmentStartIsRefusedAtThePlanMachineLimitPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_enroll_limit_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "enroll-limit-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES ($1,$2,$1,'held','linux','amd64','/w','online','occupied')`, "mch_enroll_limit_"+suffix, userID); err != nil {
		t.Fatal(err)
	}
	limits := config.Default().Billing.Limits
	limits.Machines.Free = 1
	planlimits.Configure(limits, "https://console.example.test/billing")
	t.Cleanup(func() { planlimits.Configure(config.Default().Billing.Limits, "") })
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-enrollment-key")
	_, err := service.StartEnrollment(ctx, userID, "idem-limit-"+suffix)
	var limit *planlimits.LimitError
	if !errors.As(err, &limit) || limit.Code() != "machine_limit_reached" || limit.Limit != 1 || limit.BillingURL == "" {
		t.Fatalf("enrollment at the machine limit: %v", err)
	}
	var pairings int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_pairings WHERE idempotency_key=$1`, "idem-limit-"+suffix).Scan(&pairings); err != nil || pairings != 0 {
		t.Fatalf("a refused enrollment created %d pairings (err=%v)", pairings, err)
	}
}

// limitSeatAuthorizer refuses every machine seat as the plan's machine limit would.
type limitSeatAuthorizer struct{ testSeatAuthorizer }

func (limitSeatAuthorizer) ReserveUserMachineSeat(context.Context, *db.Tx, string) error {
	return &planlimits.LimitError{Kind: planlimits.Machines, Plan: planlimits.Free, Limit: 1, Used: 1}
}

func TestPairingRefusedAtThePlanMachineLimitNamesItInTheEnrollmentPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_pair_limit_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "pair-limit-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, limitSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-enrollment-key")
	start, err := service.StartEnrollment(ctx, userID, "idem-pair-limit-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreatePairing(ctx, PairingInput{EnrollmentToken: start.BootstrapToken, Verifier: "verifier-pair-limit-" + suffix, Alias: "limit-host", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace", PublicIdentityKey: newIntegrationPublicIdentityKey(t)})
	var limit *planlimits.LimitError
	if !errors.As(err, &limit) {
		t.Fatalf("pairing past the machine limit: %v", err)
	}
	enrollment, err := service.Enrollment(ctx, userID, start.ID)
	if err != nil {
		t.Fatal(err)
	}
	if enrollment.State != "failed_retryable" || enrollment.FailureCode != "machine_limit_reached" {
		t.Fatalf("enrollment after the refusal: state=%q failure_code=%q", enrollment.State, enrollment.FailureCode)
	}
	retried, err := service.RetryEnrollment(ctx, userID, start.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.FailureCode != "" {
		t.Fatalf("a retry kept failure_code %q", retried.FailureCode)
	}
}

// P0 (2026-10-26): since the machine list stopped allocating local address leases (05862b9b), a machine that
// never connected control sync had none, so it had no address and private access to it failed (14 machines on
// production). Pairing gives the machine its lease in the pairing transaction: it is listed with its address.
func TestPairedMachineHasALocalAddressLease(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_pair_lease_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "pair-lease-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_pair_lease_"+suffix, userID, "sub_pair_lease_"+suffix); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := base64.RawURLEncoding.EncodeToString(public)
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("pair-lease-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "henr_pair_lease_" + suffix, HelperID: "helper_pair_lease_" + suffix, Credential: "pair-lease-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	start, err := service.StartEnrollmentWithOptions(ctx, userID, "idem-pair-lease-"+suffix, EnrollmentOptions{Shell: "posix"})
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: start.BootstrapToken, Verifier: "pair-lease-verifier-" + suffix, Alias: "fresh", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/paperboat", PublicIdentityKey: publicKey, RuntimeVersions: json.RawMessage(`{"pb":"test"}`)})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := service.Approve(ctx, userID, pairing.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	var slot int
	if err := store.SQL().QueryRowContext(ctx, `SELECT address_slot FROM paperboat.control_sync_leases WHERE machine_id=$1`, machine.ID).Scan(&slot); err != nil {
		t.Fatalf("the paired machine %s has no local address lease: %v", machine.ID, err)
	}
	items, _, _, err := service.ListInventory(workspace.WithContext(ctx, workspace.Scope{ID: "personal"}), userID, InventoryOptions{Limit: 10})
	if err != nil || len(items) != 1 || items[0].ID != machine.ID || items[0].AssignedIP == "" {
		t.Fatalf("machine list after pairing: %+v, %v; want the machine with its address", items, err)
	}
}

func TestObservedFileTransferModesIsAcceptedPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID := "usr_ftm_"+suffix, "um_ftm_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$1,$1||'@example.test','active')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,observed_capabilities) VALUES ($1,$2,$1,'linux','amd64','/w','online','occupied',true,ARRAY['file_receive','file_transfer_modes'])`, machineID, userID); err != nil {
		t.Fatalf("a destination observing file_transfer_modes was refused: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET observed_capabilities=ARRAY['file_receive','unknown_capability'] WHERE id=$1`, machineID); err == nil {
		t.Fatal("an unknown observed capability was accepted")
	}
}
