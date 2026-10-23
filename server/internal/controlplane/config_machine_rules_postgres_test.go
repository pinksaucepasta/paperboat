package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"strings"
	"testing"
	"time"
)

func TestConfigCustomRepositoryRulesAccessAndRevocation(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	suffix := strings.ReplaceAll(t.Name(), "/", "_")
	user, environmentID := "cfg_credential_user_"+suffix, "cfg_credential_env_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$2,$3)`, environmentID, "workspace_"+suffix, user); err != nil {
		t.Fatal(err)
	}
	helperPrivate := enrollmentTestPrivateKey(environmentID)
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,public_identity_key,seat_state) VALUES ($1,$2,$3,'machine','linux','arm64','/workspace',$4,'occupied')`, "cfg_credential_machine_"+suffix, user, environmentID, base64.RawURLEncoding.EncodeToString(helperPrivate.Public().(ed25519.PublicKey))); err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("c", ed25519.SeedSize)))
	signer, err := mint.New([]mint.Key{{ID: "config-test", PrivateKey: privateKey}}, "config-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment := NewEnrollmentService(store, signer, nil, "https://api.example.test", "config-credential-encryption-key")
	enrollment.clock = func() time.Time { return now }
	grant, err := enrollment.Issue(ctx, user, "config-enrollment-01", environmentID, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := enrollment.Exchange(ctx, grant.Credential, helperPrivate.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	assignments := NewConfigAssignmentService(store, nil, "warning-7")

	assignments.clock = func() time.Time { return now }
	repository, err := assignments.ConnectRepository(ctx, user, "git", "/srv/config-test.git", "Config")
	if err != nil {
		t.Fatal(err)
	}
	pushRepository, err := assignments.ConnectRepository(ctx, user, "git", "/srv/publish-test.git", "Publish")
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := assignments.ConfigureProjection(ctx, user, identity.MachineID, repository.ID, pushRepository.ID, ConfigModeBidirectional, false, "warning-7", 0, []ConfigPathRule{{Source: "machine", ID: "settings", RepositoryPath: "editor/settings.json", LocalPath: "/home/owner/settings.json", Kind: "file"}}, []ConfigRepositoryBinding{{RepositoryID: repository.ID, URL: "ssh://git@lan/config-test.git"}, {RepositoryID: pushRepository.ID, URL: "ssh://git@lan/publish-test.git"}}, testConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	assignment, err = assignments.AcceptConsent(ctx, user, identity.MachineID, "warning-7", assignment.Version)
	if err != nil {
		t.Fatal(err)
	}

	if repository.ProviderAccountID.Valid || repository.CredentialCapability.String != "machine_repository" || repository.DefaultBranch.String != "main" || repository.ExternalRepositoryID.String == repository.ExternalRef {
		t.Fatalf("custom repository authority incorrect: %+v", repository)
	}
	reconnected, err := assignments.ConnectRepository(ctx, user, "git", "/srv/config-test.git", "Renamed")
	if err != nil || reconnected.ID != repository.ID || reconnected.ExternalRepositoryID != repository.ExternalRepositoryID {
		t.Fatalf("canonical reconnect identity changed: %+v %v", reconnected, err)
	}
	if _, err := assignments.ConnectRepository(ctx, user, "git", "/srv/config-test.git", "Changed branch", "other"); !errors.Is(err, ErrAssignmentConflict) {
		t.Fatalf("active branch changed without revocation: %v", err)
	}
	body := []byte(`{}`)
	proofFor := func(operation, path string) []byte {
		sum := sha256.Sum256(body)
		payload, _ := json.Marshal(HelperProofClaims{HelperID: identity.HelperID, EnvironmentID: environmentID, OperationID: operation, Method: "POST", Path: path, BodySHA256: base64.RawURLEncoding.EncodeToString(sum[:]), IssuedAt: now, ExpiresAt: now.Add(time.Minute)})
		proof, _ := json.Marshal(helperProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(helperPrivate, payload))})
		return proof
	}
	credentials := NewConfigCredentialService(store, signer, "https://api.example.test", "config-credential-encryption-key")
	credentials.clock = func() time.Time { return now }
	credentials.SetWarningRevision("warning-7")
	credentials.SetMode("leased_writes")
	credential, err := credentials.Issue(ctx, identity.Credential, proofFor("custom-credential", "/v1/config/credentials"), body, "POST", "/v1/config/credentials")
	if err != nil {
		t.Fatal(err)
	}
	leases := NewConfigLeaseService(store, nil)
	leases.ConfigureAuthentication(enrollment, signer, "https://api.example.test", "warning-7")
	leases.ConfigureMode("leased_writes")
	leases.clock = func() time.Time { return now }
	access := NewConfigRepositoryAccessService(store, leases, nil, "config-credential-encryption-key", nil)
	access.clock = func() time.Time { return now }
	issue := func(operation, direction string) (ConfigRepositoryAccess, error) {
		return access.Issue(ctx, identity.Credential, credential.Credential, proofFor(operation, "/v1/config/repository-access"), body, "POST", "/v1/config/repository-access", operation, direction)
	}
	granted, err := issue("custom-access", "push")
	if err != nil {
		t.Fatal(err)
	}
	if granted.Transport != "ssh" || granted.CloneURL != "ssh://git@lan/publish-test.git" || granted.RepositoryID != pushRepository.ID || granted.Password != "" || granted.Username != "" || granted.Capability != "repository_contents_write" {
		t.Fatalf("custom alias grant = %+v", granted)
	}
	replayed, err := issue("custom-access", "push")
	if err != nil || replayed != granted {
		t.Fatalf("replay = %+v %v", replayed, err)
	}
	runtime := NewConfigRuntimeService(store, leases, config.ConfigSync{Mode: "leased_writes"})
	descriptor, err := runtime.Get(ctx, identity.Credential, credential.Credential, proofFor("custom-runtime", "/v1/config/runtime"), body, "POST", "/v1/config/runtime")
	if err != nil || len(descriptor.PathRules) != 1 || len(descriptor.RepositoryBindings) != 2 || descriptor.AssignmentVersion != assignment.Version {
		t.Fatalf("runtime machine rules = %+v %v", descriptor, err)
	}
	conflictRevision := strings.Repeat("b", 64)
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_config_sync_statuses
 (environment_id,repository_id,assignment_id,machine_id,installation_generation,warning_revision,policy_revision,sync_revision,state,remote_revision,pending_clean_path_count,conflicts,machine_updated_at,observed_at)
 VALUES ($1,$2,$3,$4,1,'warning-7','policy',1,'conflict','head',0,
 jsonb_build_array(jsonb_build_object('path','editor/settings.json','reason','concurrent_update','revision',$5::text)),$6,$6)`, environmentID, repository.ID, assignment.ID, identity.MachineID, conflictRevision, now); err != nil {
		t.Fatal(err)
	}
	conflicts := NewConfigConflictService(store, leases, nil)
	resolution, err := conflicts.Request(ctx, user, environmentID, assignment.Version, ConfigConflictResolution{Path: "editor/settings.json", ConflictRevision: conflictRevision, ExpectedRemoteRevision: "head", Scope: "path", Action: "keep_remote"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := conflicts.Pending(ctx, identity.Credential, credential.Credential, proofFor("distinct-pending", "/v1/config/conflict-resolutions"), body, "POST", "/v1/config/conflict-resolutions")
	if err != nil || len(pending) != 1 || pending[0].ID != resolution.ID {
		t.Fatalf("distinct-target conflict was not delivered: count=%d error=%v", len(pending), err)
	}
	if err := conflicts.Acknowledge(ctx, identity.Credential, credential.Credential, proofFor("distinct-ack", "/v1/config/conflict-resolutions/ack"), body, "POST", "/v1/config/conflict-resolutions/ack", resolution.ID, "landed"); err != nil {
		t.Fatalf("distinct-target resolution acknowledgement: %v", err)
	}
	pending, err = conflicts.Pending(ctx, identity.Credential, credential.Credential, proofFor("distinct-pending-after-ack", "/v1/config/conflict-resolutions"), body, "POST", "/v1/config/conflict-resolutions")
	if err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged resolution remained pending: count=%d error=%v", len(pending), err)
	}
	holder := ConfigLeaseHolder{RepositoryID: pushRepository.ID, AssignmentID: assignment.ID, EnvironmentID: environmentID, MachineID: identity.MachineID, InstallationGeneration: 1, BaseRemoteRevision: "head"}
	lease, err := leases.Acquire(ctx, "custom-lease", holder, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := assignments.ConfigureProjection(ctx, user, identity.MachineID, repository.ID, pushRepository.ID, ConfigModeBidirectional, false, "warning-7", assignment.Version, []ConfigPathRule{{Source: "machine", ID: "settings", RepositoryPath: "editor/settings.json", LocalPath: "/home/owner/renamed.json", Kind: "file"}}, nil, testConfigurationRevision)
	if err != nil || replacement.ID != assignment.ID || replacement.Version != assignment.Version+1 || replacement.ApprovedPullRevision.Valid || replacement.ConsentState != "pending" {
		t.Fatalf("mapping change failed fence: %+v %v", replacement, err)
	}
	if _, err = issue("old-custom-access", "push"); !errors.Is(err, ErrConfigRepositoryAccessInvalid) {
		t.Fatalf("old assignment retained access: %v", err)
	}
	if _, err = leases.Renew(ctx, "old-custom-lease", holder, lease.LeaseID, lease.FencingToken, 30*time.Second); !errors.Is(err, ErrConfigLeaseLost) {
		t.Fatalf("old lease retained authority: %v", err)
	}
	if _, err = assignments.ConfigureProjection(ctx, user, identity.MachineID, repository.ID, pushRepository.ID, ConfigModeBidirectional, false, "warning-7", assignment.Version, nil, nil, testConfigurationRevision); !errors.Is(err, ErrAssignmentConflict) {
		t.Fatalf("stale version accepted: %v", err)
	}
	replacement, err = assignments.AcceptConsent(ctx, user, identity.MachineID, "warning-7", replacement.Version)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := assignments.ConfigureProjection(ctx, user, identity.MachineID, repository.ID, pushRepository.ID, ConfigModeBidirectional, false, "warning-7", replacement.Version, nil, nil, testConfigurationRevision)
	if err != nil || string(empty.PathRules) != "[]" || string(empty.RepositoryBindings) != "[]" {
		t.Fatalf("empty rules must remain explicit no-op: %+v %v", empty, err)
	}
	empty, err = assignments.AcceptConsent(ctx, user, identity.MachineID, "warning-7", empty.Version)
	if err != nil {
		t.Fatalf("accept empty projection revision: %v", err)
	}

	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_config_repositories SET provider='github', external_ref=$2, display_name='owner/config-test', provider_account_id=$3, external_repository_id='123456', clone_url='https://github.com/owner/config-test.git', publish_url='https://github.com/owner/config-test.git', default_branch='main', authorization_ref=$4, credential_capability='provider_user_repository', version=version+1 WHERE id=$1`, pushRepository.ID, "provider-access-"+suffix, user, "github-user:"+user); err != nil {
		t.Fatal("provider repository fixture failed")
	}
	providerCredential, err := credentials.Issue(ctx, identity.Credential, proofFor("provider-access-credential", "/v1/config/credentials"), body, "POST", "/v1/config/credentials")
	if err != nil {
		t.Fatalf("provider access config credential: %v", err)
	}
	const secretProviderToken = "provider-token-must-not-appear"
	malformedCalls := 0
	malformedProvider := NewConfigRepositoryAccessService(store, leases, ConfigRepositoryAccessIssuerFuncs{Issue: func(_ context.Context, actor, externalID, permission string) (ScopedRepositoryCredential, error) {
		malformedCalls++
		if actor != user || externalID != "123456" || permission != "write" {
			t.Fatal("provider request did not preserve the authorized owner, repository, and permission")
		}
		return ScopedRepositoryCredential{Token: secretProviderToken, ExpiresAt: now.Add(30 * time.Second)}, nil
	}}, "config-credential-encryption-key", nil)
	malformedProvider.clock = func() time.Time { return now }
	malformedOperation := "provider-malformed-" + suffix
	_, err = malformedProvider.Issue(ctx, identity.Credential, providerCredential.Credential, proofFor(malformedOperation, "/v1/config/repository-access"), body, "POST", "/v1/config/repository-access", malformedOperation, "push")
	if !errors.Is(err, ErrConfigRepositoryAccessUncertain) || errors.Is(err, ErrConfigRepositoryAccessInvalid) || strings.Contains(err.Error(), secretProviderToken) {
		t.Fatalf("malformed provider credential outcome = %v", err)
	}
	var malformedState, malformedCode string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state, coalesce(last_error_code,'') FROM paperboat.control_config_repository_access_operations WHERE operation_id=$1`, malformedOperation).Scan(&malformedState, &malformedCode); err != nil {
		t.Fatal(err)
	}
	if malformedState != "uncertain" || malformedCode != "provider_outcome_uncertain" || malformedCalls != 1 {
		t.Fatalf("malformed provider outcome was not durably uncertain: state=%q code=%q calls=%d", malformedState, malformedCode, malformedCalls)
	}
	if _, err := malformedProvider.Issue(ctx, identity.Credential, providerCredential.Credential, proofFor(malformedOperation, "/v1/config/repository-access"), body, "POST", "/v1/config/repository-access", malformedOperation, "push"); !errors.Is(err, ErrConfigRepositoryAccessUncertain) || malformedCalls != 1 {
		t.Fatalf("malformed provider replay = %v calls=%d", err, malformedCalls)
	}

	providerFailure := errors.New("provider connection reset")
	issueCtx, cancelIssue := context.WithCancel(ctx)
	defer cancelIssue()
	failingProvider := NewConfigRepositoryAccessService(store, leases, ConfigRepositoryAccessIssuerFuncs{Issue: func(context.Context, string, string, string) (ScopedRepositoryCredential, error) {
		cancelIssue()
		return ScopedRepositoryCredential{}, providerFailure
	}}, "config-credential-encryption-key", nil)
	failingProvider.clock = func() time.Time { return now }
	failureOperation := "provider-marker-failure-" + suffix
	_, err = failingProvider.Issue(issueCtx, identity.Credential, providerCredential.Credential, proofFor(failureOperation, "/v1/config/repository-access"), body, "POST", "/v1/config/repository-access", failureOperation, "push")
	if !errors.Is(err, ErrConfigRepositoryAccessUncertain) || !errors.Is(err, providerFailure) || !errors.Is(err, context.Canceled) || errors.Is(err, ErrConfigRepositoryAccessInvalid) {
		t.Fatalf("provider cause or uncertain-marker failure was lost: %v", err)
	}
	var failedMarkerState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.control_config_repository_access_operations WHERE operation_id=$1`, failureOperation).Scan(&failedMarkerState); err != nil {
		t.Fatal(err)
	}
	if failedMarkerState != "pending" {
		t.Fatalf("failed uncertain marker changed state unexpectedly: %q", failedMarkerState)
	}
	// Unassigning forgets what the machine reported for the assignment it no longer has: the frozen conflict
	// stays out of the account status instead of showing a conflict on a machine that is Off.
	statusService := NewConfigStatusService(store, nil, nil, 10)
	conflictsOf := func() int {
		t.Helper()
		account, statusErr := statusService.Account(ctx, user)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		for _, machine := range account.Environments {
			if machine.MachineID == identity.MachineID {
				return len(machine.Conflicts)
			}
		}
		return -1
	}
	if conflictsOf() != 1 {
		t.Fatalf("precondition: the reported conflict is visible while assigned: %d", conflictsOf())
	}
	current, err := assignments.Assignment(ctx, user, identity.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	if err = assignments.Clear(ctx, user, identity.MachineID, current.Version); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if got := conflictsOf(); got > 0 {
		t.Fatalf("an unassigned machine still reports %d conflicts", got)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET installation_generation=installation_generation+1 WHERE id=$1`, identity.MachineID); err != nil {
		t.Fatal(err)
	}
	if _, err := leases.Authenticate(ctx, identity.Credential, providerCredential.Credential, proofFor("old-generation-runtime", "/v1/config/runtime"), body, "POST", "/v1/config/runtime"); !errors.Is(err, ErrConfigLeaseInvalid) {
		t.Fatalf("previous installation retained configuration authority: %v", err)
	}
}
