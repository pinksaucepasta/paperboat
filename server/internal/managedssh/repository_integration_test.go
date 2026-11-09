package managedssh

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"golang.org/x/crypto/ssh"
)

func TestSQLRepositoryManagedSSHAuthorityLifecycle(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run managed SSH repository integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + strings.ReplaceAll(now.Format("150405.000000000"), ".", "")
	userA, userB := "ssh_user_a_"+suffix, "ssh_user_b_"+suffix
	clientA, clientB := "ssh_cli_a_"+suffix, "ssh_cli_b_"+suffix
	machineA, machineB := "ssh_machine_a_"+suffix, "ssh_machine_b_"+suffix
	for _, userID := range []string{userA, userB} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+userID, userID+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []struct{ id, userID string }{{clientA, userA}, {clientB, userB}} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,$3,'SSH test','desktop','test',ARRAY['account:read'],'active',$4,$4)`, value.id, value.userID, "client_"+value.id, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []struct{ id, userID string }{{machineA, userA}, {machineB, userB}} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, value.id, value.userID, "env_"+value.id); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(repository)
	target, err := service.RegisterTarget(ctx, RegisterTargetRequest{OperationID: "operation_target_initial" + suffix, ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1, OSUser: "deploy", TargetPort: 22, Now: now})
	if err != nil || target.OSUser != "deploy" || target.TargetPort != 22 || target.ReconciliationVersion != 1 {
		t.Fatalf("initial target=%+v error=%v", target, err)
	}
	if replay, err := service.RegisterTarget(ctx, RegisterTargetRequest{OperationID: "operation_target_initial" + suffix, ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1, OSUser: "deploy", TargetPort: 22, Now: now.Add(time.Second)}); err != nil || replay != target {
		t.Fatalf("target replay=%+v error=%v", replay, err)
	}
	if _, err := service.RegisterTarget(ctx, RegisterTargetRequest{OperationID: "operation_target_initial" + suffix, ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1, OSUser: "root", TargetPort: 22, Now: now.Add(time.Second)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("target OS-user conflict=%v", err)
	}
	updatedTarget, err := service.UpdateTargetPort(ctx, UpdateTargetPortRequest{OperationID: "operation_target_update" + suffix, ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1, TargetPort: 2222, ExpectedReconciliationVersion: 1, Now: now.Add(time.Second)})
	if err != nil || updatedTarget.TargetPort != 2222 || updatedTarget.ReconciliationVersion != 2 {
		t.Fatalf("updated target=%+v error=%v", updatedTarget, err)
	}
	if _, err := service.UpdateTargetPort(ctx, UpdateTargetPortRequest{OperationID: "operation_target_update_conflict" + suffix, ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1, TargetPort: 2022, ExpectedReconciliationVersion: 1, Now: now.Add(2 * time.Second)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale target update=%v", err)
	}
	readTarget, err := service.GetTarget(ctx, GetTargetRequest{ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1})
	if err != nil || readTarget != updatedTarget {
		t.Fatalf("read target=%+v error=%v", readTarget, err)
	}

	clientPublic := publicLine(t, "ed25519")
	registered, err := service.RegisterClient(ctx, RegisterClientRequest{OperationID: "operation_client_register" + suffix, UserID: userA, CLIClientSessionID: clientA, PublicKey: clientPublic + " machine comment", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.RegisterClient(ctx, RegisterClientRequest{OperationID: "operation_client_register" + suffix, UserID: userA, CLIClientSessionID: clientA, PublicKey: clientPublic, Now: now.Add(time.Second)})
	if err != nil || replayed != registered {
		t.Fatalf("client replay=%+v error=%v", replayed, err)
	}
	if _, err := service.RegisterClient(ctx, RegisterClientRequest{OperationID: "operation_client_register" + suffix, UserID: userA, CLIClientSessionID: clientA, PublicKey: publicLine(t, "ed25519"), Now: now.Add(time.Second)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("client rotation error=%v", err)
	}
	if _, err := service.RegisterClient(ctx, RegisterClientRequest{OperationID: "operation_client_cross_owner" + suffix, UserID: userB, CLIClientSessionID: clientB, PublicKey: clientPublic, Now: now.Add(time.Second)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-owner client error=%v", err)
	}
	activeClients, err := service.ListClientKeys(ctx, ListClientKeysRequest{ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1})
	if err != nil || len(activeClients.Keys) != 1 || activeClients.Keys[0].Fingerprint != registered.Fingerprint {
		t.Fatalf("active clients=%+v error=%v", activeClients, err)
	}
	revoked, err := service.RevokeClient(ctx, RevokeClientRequest{OperationID: "operation_client_revoke" + suffix, ActorUserID: userA, Fingerprint: registered.Fingerprint, Reason: "client_logout", Now: now.Add(2 * time.Second)})
	if err != nil || revoked.State != "revoked" || revoked.ReconciliationVersion != 2 {
		t.Fatalf("revoked=%+v error=%v", revoked, err)
	}
	if replay, err := service.RevokeClient(ctx, RevokeClientRequest{OperationID: "operation_client_revoke" + suffix, ActorUserID: userA, Fingerprint: registered.Fingerprint, Reason: "client_logout", Now: now.Add(3 * time.Second)}); err != nil || !replay.RevokedAt.Equal(revoked.RevokedAt) {
		t.Fatalf("revocation replay=%+v error=%v", replay, err)
	}
	if _, err := service.RevokeClient(ctx, RevokeClientRequest{OperationID: "operation_client_revoke" + suffix, ActorUserID: userA, Fingerprint: registered.Fingerprint, Reason: "key_compromise", Now: now.Add(3 * time.Second)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("revocation reason conflict=%v", err)
	}
	activeClients, err = service.ListClientKeys(ctx, ListClientKeysRequest{ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1})
	if err != nil || len(activeClients.Keys) != 0 {
		t.Fatalf("active clients after revocation=%+v error=%v", activeClients, err)
	}

	hostOne, hostTwo := publicLine(t, "ed25519"), publicLine(t, "ed25519")
	firstRequest := ObserveHostRequest{OperationID: "operation_host_observe_initial" + suffix, UserID: userA, UserMachineID: machineA, MachineGeneration: 1, ObservationGeneration: 1, PublicKeys: []string{hostOne}, Now: now}
	first, err := service.ObserveHost(ctx, firstRequest)
	if err != nil || first.State != "active" || !first.PromotedAt.Equal(now) || len(first.Keys) != 1 {
		t.Fatalf("initial set=%+v error=%v", first, err)
	}
	activeHost, err := service.GetActiveHost(ctx, GetHostKeySetRequest{ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1})
	if err != nil || activeHost.ID != first.ID || len(activeHost.Keys) != 1 {
		t.Fatalf("active host=%+v error=%v", activeHost, err)
	}
	testSharedSSHAuthority(t, ctx, store, service, userA, userB, clientB, machineA, now)
	identicalRequest := firstRequest
	identicalRequest.OperationID, identicalRequest.ObservationGeneration = "operation_host_observe_identical"+suffix, 2
	identical, err := service.ObserveHost(ctx, identicalRequest)
	if err != nil || identical.ID != first.ID || identical.State != "active" {
		t.Fatalf("identical set=%+v error=%v", identical, err)
	}
	pendingRequest := firstRequest
	pendingRequest.OperationID, pendingRequest.ObservationGeneration, pendingRequest.PublicKeys, pendingRequest.Now = "operation_host_observe_pending"+suffix, 3, []string{hostTwo}, now.Add(time.Second)
	pending, err := service.ObserveHost(ctx, pendingRequest)
	if err != nil || pending.State != "pending" || len(pending.Keys) != 1 {
		t.Fatalf("pending set=%+v error=%v", pending, err)
	}
	blocked := pendingRequest
	blocked.OperationID, blocked.ObservationGeneration, blocked.PublicKeys = "operation_host_observe_blocked"+suffix, 4, []string{publicLine(t, "ed25519")}
	if _, err := service.ObserveHost(ctx, blocked); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending replacement error=%v", err)
	}
	crossOwner := ObserveHostRequest{OperationID: "operation_host_cross_owner" + suffix, UserID: userB, UserMachineID: machineB, MachineGeneration: 1, ObservationGeneration: 1, PublicKeys: []string{hostOne}, Now: now}
	var inUse *HostKeyInUseError
	if _, err := service.ObserveHost(ctx, crossOwner); !errors.As(err, &inUse) || inUse.MachineName != "" {
		t.Fatalf("cross-account host key error=%v (another account's machine must not be named)", err)
	}
	if _, err := service.PromoteHost(ctx, PromoteHostRequest{OperationID: "operation_host_promote" + suffix, ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 2, SetID: pending.ID, ExpectedFingerprint: pending.Fingerprint, Now: now.Add(2 * time.Second)}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale generation promotion error=%v", err)
	}
	accessGeneration := func() (generation int64) {
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='machine_access'),0)`, machineA).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		return generation
	}
	beforePromotion := accessGeneration()
	promoted, err := service.PromoteHost(ctx, PromoteHostRequest{OperationID: "operation_host_promote" + suffix, ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1, SetID: pending.ID, ExpectedFingerprint: pending.Fingerprint, Now: now.Add(2 * time.Second)})
	if err != nil || promoted.State != "active" || !promoted.PromotedAt.Equal(now.Add(2*time.Second)) || len(promoted.Keys) != 1 {
		t.Fatalf("promoted=%+v error=%v", promoted, err)
	}
	// The host learns of the trusted set at once (machine_access wakes its key reconcile): pb ssh was refused for
	// about a minute after pb ssh trust-host (hp 2026-10-27).
	if accessGeneration() <= beforePromotion {
		t.Fatal("promoting a host key set did not publish machine_access for the machine")
	}
	activeHost, err = service.GetActiveHost(ctx, GetHostKeySetRequest{ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 1})
	if err != nil || activeHost.ID != promoted.ID {
		t.Fatalf("promoted active host=%+v error=%v", activeHost, err)
	}
	if _, err := service.GetActiveHost(ctx, GetHostKeySetRequest{ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 2}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale active host error=%v", err)
	}
	stalePendingRequest := pendingRequest
	stalePendingRequest.OperationID, stalePendingRequest.ObservationGeneration, stalePendingRequest.PublicKeys = "operation_host_observe_stale"+suffix, 5, []string{publicLine(t, "ed25519")}
	stalePending, err := service.ObserveHost(ctx, stalePendingRequest)
	if err != nil || stalePending.State != "pending" {
		t.Fatalf("stale pending=%+v error=%v", stalePending, err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET installation_generation=2 WHERE id=$1`, machineA); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetTarget(ctx, GetTargetRequest{ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 2}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale-generation target read=%v", err)
	}
	reenrolledTarget, err := service.RegisterTarget(ctx, RegisterTargetRequest{OperationID: "operation_target_reenroll" + suffix, ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 2, OSUser: "paperboat", TargetPort: 2200, Now: now.Add(4 * time.Second)})
	if err != nil || reenrolledTarget.MachineGeneration != 2 || reenrolledTarget.OSUser != "paperboat" || reenrolledTarget.TargetPort != 2200 || reenrolledTarget.ReconciliationVersion != 3 {
		t.Fatalf("reenrolled target=%+v error=%v", reenrolledTarget, err)
	}
	reenrolledRequest := pendingRequest
	reenrolledRequest.OperationID, reenrolledRequest.MachineGeneration, reenrolledRequest.ObservationGeneration = "operation_host_observe_reenrolled"+suffix, 2, 1
	reenrolledRequest.PublicKeys, reenrolledRequest.Now = []string{hostTwo}, now.Add(4*time.Second)
	reenrolled, err := service.ObserveHost(ctx, reenrolledRequest)
	if err != nil || reenrolled.State != "active" || reenrolled.MachineGeneration != 2 || reenrolled.ID == activeHost.ID || reenrolled.Fingerprint != activeHost.Fingerprint {
		t.Fatalf("reenrolled=%+v error=%v", reenrolled, err)
	}
	var stalePendingState, stalePendingReason string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,rejection_reason FROM paperboat.machine_ssh_host_key_sets WHERE id=$1`, stalePending.ID).Scan(&stalePendingState, &stalePendingReason); err != nil || stalePendingState != "rejected" || stalePendingReason != "machine_reenrolled" {
		t.Fatalf("stale pending state=%q reason=%q error=%v", stalePendingState, stalePendingReason, err)
	}
	activeHost, err = service.GetActiveHost(ctx, GetHostKeySetRequest{ActorUserID: userA, UserMachineID: machineA, MachineGeneration: 2})
	if err != nil || activeHost.ID != reenrolled.ID {
		t.Fatalf("reenrolled active host=%+v error=%v", activeHost, err)
	}
	var oldState string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.machine_ssh_host_key_sets WHERE id=$1`, first.ID).Scan(&oldState); err != nil || oldState != "superseded" {
		t.Fatalf("old state=%q error=%v", oldState, err)
	}
	var leakedAudit int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_type IN ('managed_ssh_client_key','machine_ssh_host_key_set') AND (metadata::text LIKE '%' || $1 || '%' OR metadata::text LIKE '%' || $2 || '%')`, registered.PublicKey, hostOne).Scan(&leakedAudit); err != nil || leakedAudit != 0 {
		t.Fatalf("public key audit leaks=%d error=%v", leakedAudit, err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_type='machine_ssh_target' AND (metadata ? 'os_user' OR metadata::text LIKE '%paperboat%')`).Scan(&leakedAudit); err != nil || leakedAudit != 0 {
		t.Fatalf("OS-user audit leaks=%d error=%v", leakedAudit, err)
	}
	var forbiddenColumns int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='paperboat' AND table_name IN ('managed_ssh_client_keys','machine_ssh_host_key_owners','machine_ssh_host_key_sets','machine_ssh_host_keys','machine_ssh_targets') AND column_name ~ '(private|password|secret)'`).Scan(&forbiddenColumns); err != nil || forbiddenColumns != 0 {
		t.Fatalf("forbidden managed SSH columns=%d error=%v", forbiddenColumns, err)
	}

	orphanedTargetRequest := RegisterTargetRequest{OperationID: "operation_target_orphan_replay" + suffix, ActorUserID: userB, UserMachineID: machineB, MachineGeneration: 1, OSUser: "deploy", TargetPort: 2223, Now: now.Add(5 * time.Second)}
	if _, err := service.RegisterTarget(ctx, orphanedTargetRequest); err != nil {
		t.Fatalf("register replay fixture target: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `DELETE FROM paperboat.machine_ssh_targets WHERE user_machine_id=$1`, machineB); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterTarget(ctx, orphanedTargetRequest); !errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrConflict) {
		t.Fatalf("replay with missing referenced target must preserve storage cause: %v", err)
	}
}

func testSharedSSHAuthority(t *testing.T, ctx context.Context, store *db.DB, service *Service, owner, member, client, machine string, now time.Time) {
	t.Helper()
	authority := teams.NewService(store)
	teamHash := sha256.Sum256([]byte(machine))
	teamID := fmt.Sprintf("ssh-team-%x", teamHash[:12])
	team, err := authority.Create(ctx, owner, teams.CreateRequest{OperationID: "ssh-team", TeamID: teamID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',3,$3,$4)`, "billing_"+teamID, teamID, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_workspaces WHERE team_id=$1`, teamID)
	// The entitlement transition fences outstanding team decisions.
	team, err = authority.Get(ctx, owner, teamID)
	if err != nil {
		t.Fatalf("refresh paid team: %v", err)
	}
	invite, err := authority.Invite(ctx, owner, teamID, teams.InviteRequest{OperationID: "ssh-invite", ExpectedGeneration: team.Generation, AccountID: member})
	if err != nil {
		t.Fatalf("invite SSH member: %v", err)
	}
	team, err = authority.Accept(ctx, member, invite.InvitationID, teams.AcceptRequest{OperationID: "ssh-accept"})
	if err != nil {
		t.Fatalf("accept SSH membership: %v", err)
	}
	team, err = authority.Machine(ctx, owner, teamID, teams.MachineRequest{OperationID: "ssh-share", ExpectedGeneration: team.Generation, MachineID: machine, Action: "share"})
	if err != nil {
		t.Fatalf("share SSH machine: %v", err)
	}
	key, err := service.RegisterClient(ctx, RegisterClientRequest{OperationID: "shared-client" + machine, UserID: member, CLIClientSessionID: client, PublicKey: publicLine(t, "ed25519"), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	request := GetTargetRequest{ActorUserID: member, UserMachineID: machine, MachineGeneration: 1}
	if _, err := service.GetTarget(ctx, request); err == nil {
		t.Fatal("team membership implicitly grants SSH")
	}
	team, err = authority.GrantMachine(ctx, owner, teamID, teams.MachineGrantRequest{OperationID: "ssh-grant", ExpectedGeneration: team.Generation, MachineID: machine, Audience: "all_members", Capabilities: []string{"managed_ssh"}, Active: true})
	if err != nil {
		t.Fatalf("grant SSH use: %v", err)
	}
	target, err := service.GetTarget(ctx, request)
	if err != nil || target.OSUser != "deploy" || target.TargetPort != 2222 {
		t.Fatalf("shared enrolled OS user target=%+v err=%v", target, err)
	}
	if _, err := service.GetActiveHost(ctx, GetHostKeySetRequest{ActorUserID: member, UserMachineID: machine, MachineGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	keys, err := service.ListClientKeys(ctx, ListClientKeysRequest{ActorUserID: owner, UserMachineID: machine, MachineGeneration: 1})
	if err != nil || len(keys.Keys) != 1 || keys.Keys[0].Fingerprint != key.Fingerprint {
		t.Fatalf("shared SSH key projection: count=%d err=%v", len(keys.Keys), err)
	}
	if _, err := service.UpdateTargetPort(ctx, UpdateTargetPortRequest{OperationID: "shared-cannot-admin" + machine, ActorUserID: member, UserMachineID: machine, MachineGeneration: 1, TargetPort: 2200, ExpectedReconciliationVersion: 2, Now: now}); err == nil {
		t.Fatal("SSH use grant permits target administration")
	}
	team, err = authority.Machine(ctx, owner, teamID, teams.MachineRequest{OperationID: "ssh-unshare", ExpectedGeneration: team.Generation, MachineID: machine, Action: "unshare"})
	if err != nil {
		t.Fatalf("withdraw SSH sharing: %v", err)
	}
	if _, err := service.GetTarget(ctx, request); err == nil {
		t.Fatal("withdrawn SSH target remains usable")
	}
	keys, err = service.ListClientKeys(ctx, ListClientKeysRequest{ActorUserID: owner, UserMachineID: machine, MachineGeneration: 1})
	if err != nil || len(keys.Keys) != 0 {
		t.Fatalf("withdrawn shared SSH keys: count=%d err=%v", len(keys.Keys), err)
	}
	team, err = authority.Machine(ctx, owner, teamID, teams.MachineRequest{OperationID: "ssh-team-owned", ExpectedGeneration: team.Generation, MachineID: machine, Action: "transfer_to_team", Confirmation: machine})
	if err != nil {
		t.Fatalf("transfer SSH machine ownership: %v", err)
	}
	team, err = authority.Mutate(ctx, owner, teamID, teams.MutationRequest{OperationID: "ssh-admin", ExpectedGeneration: team.Generation, Action: "role", AccountID: member, Role: "admin"})
	if err != nil {
		t.Fatalf("grant SSH administration: %v", err)
	}
	registration := RegisterTargetRequest{OperationID: "shared-admin-register" + machine, ActorUserID: member, UserMachineID: machine, MachineGeneration: 1, OSUser: "deploy", TargetPort: 2222, Now: now}
	if _, err = service.RegisterTarget(ctx, registration); err != nil {
		t.Fatalf("team administrator cannot manage SSH target: %v", err)
	}
	if _, err = authority.Mutate(ctx, owner, teamID, teams.MutationRequest{OperationID: "ssh-remove-admin", ExpectedGeneration: team.Generation, Action: "remove", AccountID: member}); err != nil {
		t.Fatalf("remove SSH administrator: %v", err)
	}
	if _, err = service.RegisterTarget(ctx, registration); err == nil {
		t.Fatal("removed administrator can replay SSH management")
	}
}

func TestSQLRepositoryManagedSSHClientKeySetIsBounded(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run managed SSH repository integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := t.Context()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + strings.ReplaceAll(now.Format("150405.000000000"), ".", "")
	userID, machineID := "ssh_bounded_user_"+suffix, "ssh_bounded_machine_"+suffix
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, query := range []string{`DELETE FROM paperboat.managed_ssh_client_keys WHERE user_id=$1`, `DELETE FROM paperboat.user_machines WHERE user_id=$1`, `DELETE FROM paperboat.cli_client_sessions WHERE user_id=$1`, `DELETE FROM paperboat.users WHERE id=$1`} {
			if _, err := store.SQL().ExecContext(clean, query, userID); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+userID, userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES ($1,$2,$3,'windows','amd64','C:\\workspace','online','occupied',true,1)`, machineID, userID, "env_"+machineID); err != nil {
		t.Fatal(err)
	}
	publicKeys := make([]string, 65)
	for index := 0; index < 65; index++ {
		sessionID := fmt.Sprintf("ssh_bounded_cli_%02d_%s", index, suffix)
		createdAt := now.Add(time.Duration(index) * time.Second)
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,$3,'bounded key','desktop','windows',ARRAY['account:read'],'active',$4,$4)`, sessionID, userID, "client_"+sessionID, createdAt); err != nil {
			t.Fatal(err)
		}
		publicKeys[index] = publicLine(t, "ed25519")
		public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKeys[index]))
		if err != nil {
			t.Fatal(err)
		}
		fingerprint := sha256.Sum256(public.Marshal())
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.managed_ssh_client_keys (fingerprint,user_id,cli_client_session_id,algorithm,public_key,reconciliation_version,created_at) VALUES ($1,$2,$3,'ssh-ed25519',$4,1,$5)`, fingerprint[:], userID, sessionID, publicKeys[index], createdAt); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	set, err := service.ListClientKeys(ctx, ListClientKeysRequest{ActorUserID: userID, UserMachineID: machineID, MachineGeneration: 1})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("oversized machine key projection must fail closed: count=%d err=%v", len(set.Keys), err)
	}
	if _, err := store.SQL().ExecContext(ctx, `DELETE FROM paperboat.managed_ssh_client_keys WHERE cli_client_session_id=$1`, "ssh_bounded_cli_00_"+suffix); err != nil {
		t.Fatal(err)
	}
	set, err = service.ListClientKeys(ctx, ListClientKeysRequest{ActorUserID: userID, UserMachineID: machineID, MachineGeneration: 1})
	if err != nil || len(set.Keys) != 64 {
		t.Fatalf("keys=%d err=%v", len(set.Keys), err)
	}
	if set.Keys[0].PublicKey != publicKeys[64] || set.Keys[63].PublicKey != publicKeys[1] {
		t.Fatal("bounded client-key order does not match creation times")
	}
	registrationSession := "ssh_bounded_cli_registration_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,$3,'bounded registration','desktop','windows',ARRAY['machines:connect'],'active',$4,$4)`, registrationSession, userID, "client_"+registrationSession, now.Add(65*time.Second)); err != nil {
		t.Fatal(err)
	}
	registrationKey := publicLine(t, "ed25519")
	if _, err := service.RegisterClient(ctx, RegisterClientRequest{OperationID: "operation_bounded_capacity_" + suffix, UserID: userID, CLIClientSessionID: registrationSession, PublicKey: registrationKey, Now: now.Add(65 * time.Second)}); !errors.Is(err, ErrClientKeyCapacity) {
		t.Fatalf("registration over account key capacity error=%v, want the typed capacity refusal", err)
	}
	// A signed-out (revoked) session frees its slot at once: dead sessions cannot hold the cap (hp 2026-10-27,
	// a full test account refused every new computer's key).
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=$2 WHERE id=$1`, "ssh_bounded_cli_01_"+suffix, now.Add(66*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterClient(ctx, RegisterClientRequest{OperationID: "operation_bounded_capacity_freed_" + suffix, UserID: userID, CLIClientSessionID: registrationSession, PublicKey: registrationKey, Now: now.Add(67 * time.Second)}); err != nil {
		t.Fatalf("registration after a session was revoked: %v", err)
	}
}

func TestSQLRepositoryManagedSSHKeyRevokesWithClientSessionAndAccount(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run managed SSH repository integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + strings.ReplaceAll(now.Format("150405.000000000"), ".", "")
	type authority struct{ userID, clientID, machineID string }
	authorities := []authority{
		{userID: "ssh_session_revoke_user_" + suffix, clientID: "ssh_session_revoke_client_" + suffix, machineID: "ssh_session_revoke_machine_" + suffix},
		{userID: "ssh_account_revoke_user_" + suffix, clientID: "ssh_account_revoke_client_" + suffix, machineID: "ssh_account_revoke_machine_" + suffix},
	}
	for _, value := range authorities {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, value.userID, "workos_"+value.userID, value.userID+"@example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,$3,'SSH revocation test','desktop','test',ARRAY['machines:connect'],'active',$4,$4)`, value.clientID, value.userID, "client_"+value.clientID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, value.machineID, value.userID, "env_"+value.machineID); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]ClientKey, len(authorities))
	for index, value := range authorities {
		key, err := service.RegisterClient(ctx, RegisterClientRequest{OperationID: "operation_register_" + value.clientID, UserID: value.userID, CLIClientSessionID: value.clientID, PublicKey: publicLine(t, "ed25519"), Now: now})
		if err != nil {
			t.Fatal(err)
		}
		keys[index] = key
	}

	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.cli_client_sessions SET state='revoked', revoked_at=$2, revocation_reason='client_logout' WHERE id=$1`, authorities[0].clientID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.users SET status='suspended' WHERE id=$1`, authorities[1].userID); err != nil {
		t.Fatal(err)
	}
	for index, wantReason := range []string{"client_logout", "account_revoked"} {
		var state, reason string
		var revokedAt time.Time
		var version int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT state,revocation_reason,revoked_at,reconciliation_version FROM paperboat.managed_ssh_client_keys WHERE fingerprint=$1`, keys[index].Fingerprint[:]).Scan(&state, &reason, &revokedAt, &version); err != nil {
			t.Fatal(err)
		}
		if state != "revoked" || reason != wantReason || revokedAt.IsZero() || version != 2 {
			t.Fatalf("managed key %d state=%q reason=%q revoked_at=%s version=%d", index, state, reason, revokedAt, version)
		}
		set, err := service.ListClientKeys(ctx, ListClientKeysRequest{ActorUserID: authorities[index].userID, UserMachineID: authorities[index].machineID, MachineGeneration: 1})
		if index == 0 && (err != nil || len(set.Keys) != 0) {
			t.Fatalf("revoked session keys=%+v error=%v", set, err)
		}
		if index == 1 && !errors.Is(err, ErrUnavailable) {
			t.Fatalf("suspended account managed authority error=%v", err)
		}
	}
}

// A CLI session that has not been used within the configured window stops counting toward SSH
// client-key capacity and authorized keys; it never blocks a new registration.
func TestSQLRepositoryManagedSSHIdleClientSessionsDoNotCount(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run managed SSH repository integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := t.Context()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + strings.ReplaceAll(now.Format("150405.000000000"), ".", "")
	userID, machineID := "ssh_idle_user_"+suffix, "ssh_idle_machine_"+suffix
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, query := range []string{`DELETE FROM paperboat.managed_ssh_client_keys WHERE user_id=$1`, `DELETE FROM paperboat.user_machines WHERE user_id=$1`, `DELETE FROM paperboat.cli_client_sessions WHERE user_id=$1`, `DELETE FROM paperboat.users WHERE id=$1`} {
			if _, err := store.SQL().ExecContext(clean, query, userID); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+userID, userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/w','online','occupied',true,1)`, machineID, userID, "env_"+machineID); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 70; index++ {
		sessionID := fmt.Sprintf("ssh_idle_cli_%02d_%s", index, suffix)
		idle := now.Add(-90 * 24 * time.Hour)
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,last_used_at) VALUES ($1,$2,$3,'idle','desktop','test',ARRAY['machines:connect'],'active',$4,$4,$4)`, sessionID, userID, "client_"+sessionID, idle); err != nil {
			t.Fatal(err)
		}
		public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicLine(t, "ed25519")))
		if err != nil {
			t.Fatal(err)
		}
		fingerprint := sha256.Sum256(public.Marshal())
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.managed_ssh_client_keys (fingerprint,user_id,cli_client_session_id,algorithm,public_key,reconciliation_version,created_at) VALUES ($1,$2,$3,'ssh-ed25519',$4,1,$5)`, fingerprint[:], userID, sessionID, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public))), idle); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	set, err := service.ListClientKeys(ctx, ListClientKeysRequest{ActorUserID: userID, UserMachineID: machineID, MachineGeneration: 1})
	if err != nil || len(set.Keys) != 0 {
		t.Fatalf("70 idle sessions counted: keys=%d err=%v", len(set.Keys), err)
	}
	fresh := "ssh_idle_cli_fresh_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,last_used_at) VALUES ($1,$2,$3,'fresh','desktop','test',ARRAY['machines:connect'],'active',$4,$4,$4)`, fresh, userID, "client_"+fresh, now); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterClient(ctx, RegisterClientRequest{OperationID: "operation_idle_register_" + suffix, UserID: userID, CLIClientSessionID: fresh, PublicKey: publicLine(t, "ed25519"), Now: now}); err != nil {
		t.Fatalf("registration blocked by idle sessions: %v", err)
	}
	set, err = service.ListClientKeys(ctx, ListClientKeysRequest{ActorUserID: userID, UserMachineID: machineID, MachineGeneration: 1})
	if err != nil || len(set.Keys) != 1 {
		t.Fatalf("keys after fresh registration=%d err=%v", len(set.Keys), err)
	}
}
