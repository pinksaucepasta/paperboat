package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConfigRepositoryLeaseSerializesFencesAndRevokes(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(t.Name(), "/", "_")
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	user := "lease_user_" + suffix
	repositoryID := "lease_repo_" + suffix
	environmentA, environmentB := "lease_env_a_"+suffix, "lease_env_b_"+suffix
	helperA, helperB := "lease_helper_a_"+suffix, "lease_helper_b_"+suffix
	assignmentA, assignmentB := "lease_assignment_a_"+suffix, "lease_assignment_b_"+suffix

	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupConfigLeaseFixture(store, user, repositoryID, environmentA, environmentB)
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_config_repositories (id,owner_user_id,provider,external_ref,display_name) VALUES ($1,$2,'github',$3,'Config')`, repositoryID, user, repositoryID); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct{ environment, helper, assignment string }{
		{environmentA, helperA, assignmentA}, {environmentB, helperB, assignmentB},
	} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$2,$3)`, fixture.environment, "workspace_"+fixture.environment, user); err != nil {
			t.Fatal(err)
		}
		machineID := "machine_" + fixture.environment
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root) VALUES ($1,$2,$3,'linux','unknown','/workspace')`, machineID, user, fixture.environment); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state,generation) VALUES ($1,$2,'active',1)`, fixture.helper, fixture.environment); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_config_assignments (id,machine_id,environment_id,repository_id,mode,consent_state,warning_revision,accepted_at,configuration_revision) VALUES ($1,$2,$3,$4,'bidirectional','accepted','warning-1',now(),repeat('a',64))`, fixture.assignment, machineID, fixture.environment, repositoryID); err != nil {
			t.Fatal(err)
		}
	}
	service := NewConfigLeaseService(store, nil)
	service.ConfigureMode("leased_writes")
	service.clock = func() time.Time { return now }
	holderA := ConfigLeaseHolder{RepositoryID: repositoryID, AssignmentID: assignmentA, EnvironmentID: environmentA, MachineID: "machine_" + environmentA, InstallationGeneration: 1, BaseRemoteRevision: "head-1"}
	holderB := ConfigLeaseHolder{RepositoryID: repositoryID, AssignmentID: assignmentB, EnvironmentID: environmentB, MachineID: "machine_" + environmentB, InstallationGeneration: 1, BaseRemoteRevision: "head-1"}

	first, err := service.Acquire(ctx, "acquire-a", holderA, 30*time.Second)
	if err != nil || first.FencingToken != 1 {
		t.Fatalf("first lease = %#v, %v", first, err)
	}
	replayed, err := service.Acquire(ctx, "acquire-a", holderA, 30*time.Second)
	if err != nil || replayed != first {
		t.Fatalf("acquire replay = %#v, %v", replayed, err)
	}
	if _, err := service.Acquire(ctx, "acquire-b-busy", holderB, 30*time.Second); !errors.Is(err, ErrConfigLeaseBusy) {
		t.Fatalf("competing acquire error = %v", err)
	}
	renewHolder := holderA
	renewHolder.BaseRemoteRevision = ""
	renewed, err := service.Renew(ctx, "renew-a", renewHolder, first.LeaseID, first.FencingToken, time.Minute)
	if err != nil || !renewed.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("renewed lease = %#v, %v", renewed, err)
	}
	replayedRenewal, err := service.Renew(ctx, "renew-a", renewHolder, first.LeaseID, first.FencingToken, time.Minute)
	if err != nil || replayedRenewal != renewed {
		t.Fatalf("renew replay = %#v, %v", replayedRenewal, err)
	}
	legacyOperation := "legacy-incomplete"
	if _, err := store.SQL().ExecContext(ctx, `
		INSERT INTO paperboat.control_config_repository_lease_operations
			(operation_id,operation_type,request_hash,repository_id,lease_id,fencing_token,result_state,expires_at)
		VALUES ($1,'acquire',$2,$3,$4,$5,'acquired',$6)`,
		legacyOperation, configLeaseRequestHash("acquire", legacyOperation, holderA, "", 0, 30*time.Second),
		repositoryID, first.LeaseID, first.FencingToken, first.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Acquire(ctx, legacyOperation, holderA, 30*time.Second); !errors.Is(err, ErrConfigLeaseReplay) {
		t.Fatalf("incomplete legacy replay error = %v", err)
	}

	now = now.Add(2 * time.Minute)
	second, err := service.Acquire(ctx, "acquire-b", holderB, 30*time.Second)
	if err != nil || second.FencingToken != first.FencingToken+1 {
		t.Fatalf("second lease = %#v, %v", second, err)
	}
	if err := service.Release(ctx, "release-stale", holderA, first.LeaseID, first.FencingToken); !errors.Is(err, ErrConfigLeaseLost) {
		t.Fatalf("stale release error = %v", err)
	}

	assignments := NewConfigAssignmentService(store, nil, "warning-1")
	assignments.clock = func() time.Time { return now }
	if err := assignments.Clear(ctx, user, "machine_"+environmentB, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(ctx, "renew-revoked", holderB, second.LeaseID, second.FencingToken, 30*time.Second); !errors.Is(err, ErrConfigLeaseLost) {
		t.Fatalf("revoked renew error = %v", err)
	}

	third, err := service.Acquire(ctx, "acquire-a-after-release", holderA, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_environments SET desired_state='suspended' WHERE id=$1`, environmentA); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(ctx, "renew-after-environment-suspend", holderA, third.LeaseID, third.FencingToken, 30*time.Second); !errors.Is(err, ErrConfigLeaseLost) {
		t.Fatalf("environment lifecycle trigger did not revoke lease: %v", err)
	}
	if _, err := service.Acquire(ctx, "acquire-after-environment-suspend", holderA, 30*time.Second); !errors.Is(err, ErrConfigLeaseInvalid) {
		t.Fatalf("suspended environment reacquired authority: %v", err)
	}
}

func TestConfigSyncAccountSuspensionRevokesAndPreventsAuthority(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(t.Name(), "/", "_")
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	user := "account_revoke_user_" + suffix
	repositoryID := "account_revoke_repo_" + suffix
	environmentID := "account_revoke_env_" + suffix
	helperID := "account_revoke_helper_" + suffix
	assignmentID := "account_revoke_assignment_" + suffix
	t.Cleanup(func() {
		cleanupConfigLeaseFixture(store, user, repositoryID, environmentID)
	})

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, []any{user, "workos_" + user, user + "@example.test"}},
		{`INSERT INTO paperboat.control_config_repositories (id,owner_user_id,provider,external_ref,display_name) VALUES ($1,$2,'github',$1,'Config')`, []any{repositoryID, user}},
		{`INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$2,$3)`, []any{environmentID, "workspace_" + suffix, user}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root) VALUES ($1,$2,$3,'linux','unknown','/workspace')`, []any{"machine_" + suffix, user, environmentID}},
		{`INSERT INTO paperboat.control_helpers (id,environment_id,state,generation) VALUES ($1,$2,'active',1)`, []any{helperID, environmentID}},
		{`INSERT INTO paperboat.control_config_assignments (id,machine_id,environment_id,repository_id,mode,consent_state,warning_revision,accepted_at,configuration_revision) VALUES ($1,$2,$3,$4,'bidirectional','accepted','warning-1',now(),repeat('a',64))`, []any{assignmentID, "machine_" + suffix, environmentID, repositoryID}},
		{`INSERT INTO paperboat.control_config_credentials (jti,operation_key,request_hash,environment_id,machine_id,assignment_id,warning_revision,credential_ciphertext,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, []any{"account-jti-" + suffix, "account-credential-" + suffix, []byte("request"), environmentID, "machine_" + suffix, assignmentID, "warning-1", []byte("ciphertext"), now.Add(time.Hour)}},
		{`INSERT INTO paperboat.control_config_repository_access_operations (operation_id,request_hash,repository_id,assignment_id,environment_id,machine_id,installation_generation,warning_revision,state,access_ciphertext,expires_at) VALUES ($1,$2,$3,$4,$5,$6,1,'warning-1','issued',$7,$8)`, []any{"account-access-" + suffix, []byte("request"), repositoryID, assignmentID, environmentID, "machine_" + suffix, []byte("ciphertext"), now.Add(time.Hour)}},
	}
	for _, statement := range statements {
		if _, err := store.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	service := NewConfigLeaseService(store, nil)
	service.ConfigureMode("leased_writes")
	service.clock = func() time.Time { return now }
	holder := ConfigLeaseHolder{RepositoryID: repositoryID, AssignmentID: assignmentID, EnvironmentID: environmentID, MachineID: "machine_" + suffix, InstallationGeneration: 1}
	lease, err := service.Acquire(ctx, "account-acquire-"+suffix, holder, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.users SET status='suspended' WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	var credentialRevoked, accessRevoked, leaseRevoked bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT revoked_at IS NOT NULL FROM paperboat.control_config_credentials WHERE environment_id=$1`, environmentID).Scan(&credentialRevoked); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state='revoked' AND revoked_at IS NOT NULL FROM paperboat.control_config_repository_access_operations WHERE environment_id=$1`, environmentID).Scan(&accessRevoked); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT lease_id IS NULL AND revoked_at IS NOT NULL FROM paperboat.control_config_repository_lease_authority WHERE repository_id=$1`, repositoryID).Scan(&leaseRevoked); err != nil {
		t.Fatal(err)
	}
	if !credentialRevoked || !accessRevoked || !leaseRevoked {
		t.Fatalf("account revocation credential=%t access=%t lease=%t", credentialRevoked, accessRevoked, leaseRevoked)
	}
	if _, err := service.Renew(ctx, "account-renew-after-suspend-"+suffix, holder, lease.LeaseID, lease.FencingToken, 30*time.Second); !errors.Is(err, ErrConfigLeaseLost) {
		t.Fatalf("suspended account retained lease authority: %v", err)
	}
	if _, err := service.Acquire(ctx, "account-acquire-after-suspend-"+suffix, holder, 30*time.Second); !errors.Is(err, ErrConfigLeaseInvalid) {
		t.Fatalf("suspended account reacquired authority: %v", err)
	}
}

func cleanupConfigLeaseFixture(store interface {
	SQL() *sql.DB
}, userID, repositoryID string, environmentIDs ...string) {
	ctx := context.Background()
	for _, environmentID := range environmentIDs {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_config_credentials WHERE environment_id=$1`, environmentID)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_config_repository_access_operations WHERE environment_id=$1`, environmentID)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_config_assignments WHERE environment_id=$1`, environmentID)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_helpers WHERE environment_id=$1`, environmentID)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_environments WHERE id=$1`, environmentID)
	}
	_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_config_repository_lease_operations WHERE repository_id=$1`, repositoryID)
	_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_config_repository_lease_authority WHERE repository_id=$1`, repositoryID)
	_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_config_repositories WHERE id=$1`, repositoryID)
	_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, userID)
}

func TestConfigRepositoryLeaseRejectsUnsafeRequests(t *testing.T) {
	service := NewConfigLeaseService(nil, nil)
	holder := ConfigLeaseHolder{RepositoryID: "repo", AssignmentID: "assignment", EnvironmentID: "environment", MachineID: "helper", InstallationGeneration: 1}
	for _, ttl := range []time.Duration{time.Second, 3 * time.Minute} {
		if _, err := service.Acquire(context.Background(), "operation", holder, ttl); !errors.Is(err, ErrConfigLeaseInvalid) {
			t.Fatalf("ttl %s error = %v", ttl, err)
		}
	}
}

func TestConfigLeaseUsesDistinctPushRepository(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := strings.ToLower(t.Name())
	user, environment := "user_"+suffix, "env_"+suffix
	pull, push := "pull_"+suffix, "push_"+suffix
	machine, assignment := "machine_"+suffix, "assignment_"+suffix
	t.Cleanup(func() {
		cleanupConfigLeaseFixture(store, user, push, environment)
	})
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$1,$2,'active')`, []any{user, user + "@example.test"}},
		{`INSERT INTO paperboat.control_config_repositories (id,owner_user_id,provider,external_ref,display_name) VALUES ($1,$3,'git',$1,'Pull'),($2,$3,'git',$2,'Push')`, []any{pull, push, user}},
		{`INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$1,$2)`, []any{environment, user}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root) VALUES ($1,$2,$3,'linux','unknown','/workspace')`, []any{machine, user, environment}},
		{`INSERT INTO paperboat.control_config_assignments (id,machine_id,environment_id,repository_id,push_repository_id,mode,consent_state,warning_revision,accepted_at,configuration_revision) VALUES ($1,$2,$3,$4,$5,'bidirectional','accepted','warning',now(),repeat('a',64))`, []any{assignment, machine, environment, pull, push}},
	}
	for _, statement := range statements {
		if _, err := store.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	service := NewConfigLeaseService(store, nil)
	service.ConfigureMode("leased_writes")
	holder := ConfigLeaseHolder{RepositoryID: push, AssignmentID: assignment, EnvironmentID: environment, MachineID: machine, InstallationGeneration: 1, BaseRemoteRevision: "push-head"}
	wrong := holder
	wrong.RepositoryID = pull
	if _, err := service.Acquire(ctx, "pull-lease-"+suffix, wrong, 30*time.Second); !errors.Is(err, ErrConfigLeaseInvalid) {
		t.Fatalf("pull repository acquired publication authority: %v", err)
	}
	lease, err := service.Acquire(ctx, "push-lease-"+suffix, holder, 30*time.Second)
	if err != nil || lease.RepositoryID != push {
		t.Fatalf("push lease failed: %v", err)
	}
	if _, err := service.Renew(ctx, "push-renew-"+suffix, holder, lease.LeaseID, lease.FencingToken, time.Minute); err != nil {
		t.Fatalf("push renewal failed: %v", err)
	}
	if err := service.Release(ctx, "push-release-"+suffix, holder, lease.LeaseID, lease.FencingToken); err != nil {
		t.Fatalf("push release failed: %v", err)
	}
	lease, err = service.Acquire(ctx, "push-reacquire-"+suffix, holder, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_config_repositories SET state='disconnected',disconnected_at=now() WHERE id=$1`, push); err != nil {
		t.Fatal(err)
	}
	var revoked bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT lease_id IS NULL AND revoked_at IS NOT NULL FROM paperboat.control_config_repository_lease_authority WHERE repository_id=$1`, push).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("push disconnect did not revoke the held lease: revoked=%t error=%v", revoked, err)
	}
	if _, err := service.Renew(ctx, "disconnected-renew-"+suffix, holder, lease.LeaseID, lease.FencingToken, time.Minute); !errors.Is(err, ErrConfigLeaseLost) {
		t.Fatalf("disconnected push renewed authority: %v", err)
	}
	if _, err := service.Acquire(ctx, "disconnected-push-"+suffix, holder, 30*time.Second); !errors.Is(err, ErrConfigLeaseInvalid) {
		t.Fatalf("disconnected push acquired authority: %v", err)
	}
}
