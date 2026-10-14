package environment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

func TestPostgresVaultScopesTeamRotationAndReconciliation(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN for isolated vault scope transactions")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(dsn), "_test") {
		t.Fatal("isolated test database required")
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Fresh-schema setup crosses the remote database link for historical migrations;
	// it does not consume the bounded behavior scenario below.
	setup, setupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	err = db.Migrate(setup, store)
	setupCancel()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	suffix := strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	owner, member, team, machine := "vsowner_"+suffix, "vsmember_"+suffix, "vsteam_"+suffix, "vshost_"+suffix
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// Only the validated isolated fixture database is eligible for audit cleanup.
		err := store.InTx(cleanup, func(ctx context.Context, tx *db.Tx) error {
			if _, err := tx.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`); err != nil {
				return err
			}
			for _, q := range []string{`DELETE FROM audit_events WHERE actor_user_id IN($1,$2) AND resource_id=$3`, `DELETE FROM environment_vault_scopes WHERE owner_id IN($1,$2,$3)`, `DELETE FROM billing_workspaces WHERE team_id=$3 AND $1<>'' AND $2<>''`, `DELETE FROM teams WHERE team_id=$3 AND owner_account IN($1,$2)`, `DELETE FROM users WHERE id IN($1,$2) AND $3<>''`} {
				if _, err := tx.Exec(ctx, q, owner, member, team); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
			return err
		})
		if err != nil {
			t.Errorf("cleanup exact test fixtures: %v", err)
		}
	}()
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active'),($2,$2,$2||'@invalid.test','active')`, owner, member); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state) VALUES ($1,$2,$3,'vault scopes','linux','amd64','/vault-test','online','released')`, machine, owner, "env_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, nil, "https://control.example")
	first := passwordVaultFixture(t, "https://control.example", owner, 1, make([]byte, 32))
	memberRaw := passwordVaultFixture(t, "https://control.example", member, 1, make([]byte, 32))
	if _, err := service.PutPasswordVault(ctx, owner, first); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutPasswordVault(ctx, member, memberRaw); err != nil {
		t.Fatal(err)
	}
	writer, _ := ParsePasswordVaultMetadata(first, "https://control.example", owner)
	recipient, _ := ParsePasswordVaultMetadata(memberRaw, "https://control.example", member)
	firstDigest := sha256.Sum256(first)
	nextRaw := passwordVaultFixtureByte(t, "https://control.example", owner, 2, firstDigest[:], 9)
	next, _ := ParsePasswordVaultMetadata(nextRaw, "https://control.example", owner)
	scopeRaw := vaultScopeFixture(t, fixtureScopeHeader(team, "team", owner, 1, 1, 1, make([]byte, 32)), 11)
	create := VaultTeamCreate{OperationID: "create_" + suffix, TeamID: team, VaultEnvelope: b64Vault(nextRaw), ScopeEnvelope: b64Vault(scopeRaw)}
	create.GrantEnvelope = b64Vault(vaultGrantFixture(t, fixtureGrantClaims(team, create.OperationID, 1, 1, writer, next), 11))
	// An ordinary Team without an ENV vault is absent, not an epoch conflict.
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetVaultTeam(ctx, owner, team); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owned Team without ENV: %v", err)
	}
	if _, err := service.GetVaultTeam(ctx, member, team); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign absent Team ENV: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `DELETE FROM teams WHERE team_id=$1 AND owner_account=$2`, team, owner); err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateVaultTeam(ctx, owner, create)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateVaultTeam(ctx, owner, create); err != nil {
		t.Fatal("exact initialize retry failed", err)
	}
	if created.Generation != 1 {
		t.Fatal("new team generation", created.Generation)
	}
	if created.Entitled {
		t.Fatal("new unpaid team reported entitled")
	}
	if _, err := service.GetVaultScope(ctx, owner, team, "team", team, ""); !errors.Is(err, ErrTeamEntitlementRequired) {
		t.Fatalf("unpaid authorized owner scope error: %v", err)
	}
	if _, err := service.GetVaultScope(ctx, member, team, "team", team, ""); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatalf("unpaid outsider scope error: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,period_start,period_end) VALUES($1,$2,'active',now()-interval '1 hour',now()+interval '1 hour')`, "vault_billing_"+suffix, team); err != nil {
		t.Fatal(err)
	}
	authority := teams.NewService(store)
	paidTeam, err := authority.Get(ctx, owner, team)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.teams SET suspended=true,suspended_at=now(),suspended_by=$2 WHERE team_id=$1`, team, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = service.GetVaultTeam(ctx, owner, team); !errors.Is(err, ErrNotFound) {
		t.Fatalf("suspended team ENV read err=%v", err)
	}
	if _, err = authority.Invite(ctx, owner, team, teams.InviteRequest{OperationID: "suspended_invite_" + suffix, ExpectedGeneration: paidTeam.Generation, AccountID: member}); !errors.Is(err, teams.ErrSuspended) {
		t.Fatalf("suspended team mutation err=%v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.teams SET suspended=false,suspended_at=NULL,suspended_by=NULL WHERE team_id=$1`, team); err != nil {
		t.Fatal(err)
	}
	if _, err = service.GetVaultTeam(ctx, owner, team); err != nil {
		t.Fatalf("resumed team ENV read err=%v", err)
	}
	if _, err := service.GetVaultScope(ctx, member, team, "team", team, ""); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatal("ungranted account read team", err)
	}
	invitation, err := authority.Invite(ctx, owner, team, teams.InviteRequest{OperationID: "invite_" + suffix, ExpectedGeneration: paidTeam.Generation, AccountID: member})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := authority.Accept(ctx, member, invitation.InvitationID, teams.AcceptRequest{OperationID: "accept_" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	permitted, err := authority.Grant(ctx, owner, team, teams.GrantRequest{OperationID: "permission_" + suffix, ExpectedGeneration: accepted.Generation, AccountID: member, ResourceKind: "env", ResourceID: team, Permission: "read", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	grant := VaultMemberGrant{OperationID: "grant_" + suffix, AccountID: member, ExpectedTeamGeneration: permitted.Generation, ExpectedMembershipGeneration: 1}
	grant.GrantEnvelope = b64Vault(vaultGrantFixture(t, fixtureGrantClaims(team, grant.OperationID, 1, 1, next, recipient), 11))
	granted, err := service.GrantVaultTeamMember(ctx, owner, team, grant)
	if err != nil {
		t.Fatal(err)
	}
	if granted.Generation != permitted.Generation+1 {
		t.Fatal("membership generation did not commit")
	}
	// Older snapshots must not resurrect stale authority after a later team mutation.
	if _, err := service.CreateVaultTeam(ctx, owner, create); !errors.Is(err, ErrVersionConflict) {
		t.Fatal("stale initialization replay accepted", err)
	}
	inbox, err := service.ListVaultGrants(ctx, member)
	if err != nil || len(inbox) != 1 {
		t.Fatal("missing individual grant", err)
	}
	setPaidPeriod := func(end string) {
		t.Helper()
		tx, err := store.SQL().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `ALTER TABLE paperboat.billing_workspaces DISABLE TRIGGER billing_workspace_team_entitlement`); err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET period_end=`+end+` WHERE team_id=$1`, team)
		}
		if _, enableErr := tx.ExecContext(ctx, `ALTER TABLE paperboat.billing_workspaces ENABLE TRIGGER billing_workspace_team_entitlement`); err == nil {
			err = enableErr
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	setPaidPeriod(`now()-interval '1 second'`)
	if expired, err := service.ListVaultGrants(ctx, member); err != nil || len(expired) != 0 {
		t.Fatalf("expired team delivered pending grant: count=%d err=%v", len(expired), err)
	}
	if err := service.AcknowledgeVaultGrant(ctx, member, inbox[0].DocumentID, DocumentID(memberRaw)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired team acknowledged pending grant: %v", err)
	}
	setPaidPeriod(`now()+interval '1 hour'`)
	if err := service.AcknowledgeVaultGrant(ctx, member, inbox[0].DocumentID, DocumentID(memberRaw)); err != nil {
		t.Fatal("current addressed grant acknowledgement failed", err)
	}
	if currentGrants, err := service.ListVaultGrants(ctx, member); err != nil || len(currentGrants) != 1 || !currentGrants[0].Acknowledged || currentGrants[0].DocumentID != inbox[0].DocumentID {
		t.Fatal("acknowledged current grant disappeared from source discovery", err)
	}
	if _, err := service.GetVaultScope(ctx, member, team, "team", team, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutVaultScope(ctx, member, team, "team", team, "", VaultScopePut{OperationID: "denied_write_" + suffix, Envelope: b64Vault(scopeRaw)}); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatal("read-only member wrote scope", err)
	}
	deniedGrant := grant
	deniedGrant.OperationID = "denied_grant_" + suffix
	deniedGrant.ExpectedTeamGeneration = granted.Generation
	if _, err := service.GrantVaultTeamMember(ctx, member, team, deniedGrant); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatal("member regranted key", err)
	}
	if _, err := service.RotateVaultTeam(ctx, member, team, VaultTeamRotate{OperationID: "denied_rotate_" + suffix, ExpectedTeamGeneration: granted.Generation, RemoveAccountIDs: []string{}, GrantEnvelopes: []string{}, ScopeEnvelope: b64Vault(scopeRaw)}); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatal("member rotated key", err)
	}
	writable, err := authority.Grant(ctx, owner, team, teams.GrantRequest{OperationID: "write_permission_" + suffix, ExpectedGeneration: granted.Generation, AccountID: member, ResourceKind: "env", ResourceID: team, Permission: "write", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	initialDigest := sha256.Sum256(scopeRaw)
	memberScope := vaultScopeFixture(t, fixtureScopeHeader(team, "team", member, 1, 1, 2, initialDigest[:]), 11)
	if _, err := service.PutVaultScope(ctx, member, team, "team", team, "", VaultScopePut{OperationID: "member_write_" + suffix, Envelope: b64Vault(memberScope)}); err != nil {
		t.Fatal("explicit write-granted member denied", err)
	}
	scopeRaw = memberScope
	granted.Generation = writable.Generation
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error { return db.FenceRemovedMachineVaultTx(ctx, tx, member) }); err != nil {
		t.Fatal(err)
	}
	memberInventory, err := service.GetVaultPersonalScopes(ctx, member)
	if err != nil || memberInventory.RotationRequired {
		t.Fatal("machine removal required personal key rotation", err)
	}
	fencedTeam, err := service.GetVaultTeam(ctx, owner, team)
	if err != nil || fencedTeam.RotationRequired || fencedTeam.Generation != granted.Generation+1 {
		t.Fatal("machine removal must preserve ready team keys and advance delivery authority", err)
	}
	removedTeam, err := authority.Mutate(ctx, owner, team, teams.MutationRequest{OperationID: "remove_before_rekey_" + suffix, ExpectedGeneration: fencedTeam.Generation, Action: "remove", AccountID: member})
	if err != nil {
		t.Fatal("shared membership removal failed", err)
	}
	fencedTeam, err = service.GetVaultTeam(ctx, owner, team)
	if err != nil || !fencedTeam.RotationRequired || fencedTeam.Generation != removedTeam.Generation {
		t.Fatal("shared removal lost pending rekey", err)
	}
	if _, err := service.PutVaultScope(ctx, owner, team, "team", team, "", VaultScopePut{OperationID: "blocked_" + suffix, Envelope: b64Vault(scopeRaw)}); !errors.Is(err, ErrVaultRotationRequired) {
		t.Fatal("unrotated team write accepted", err)
	}
	nextDigest := sha256.Sum256(nextRaw)
	thirdRaw := passwordVaultFixtureByte(t, "https://control.example", owner, 3, nextDigest[:], 10)
	third, _ := ParsePasswordVaultMetadata(thirdRaw, "https://control.example", owner)
	scopeDigest := sha256.Sum256(scopeRaw)
	rotatedRaw := vaultScopeFixture(t, fixtureScopeHeader(team, "team", owner, 2, 2, 3, scopeDigest[:]), 11)
	rotation := VaultTeamRotate{OperationID: "rotate_" + suffix, ExpectedTeamGeneration: fencedTeam.Generation, RemoveAccountIDs: []string{member}, ScopeEnvelope: b64Vault(rotatedRaw), VaultEnvelope: b64Vault(thirdRaw), GrantEnvelopes: []string{}}
	if _, err := service.RotateVaultTeam(ctx, owner, team, rotation); !errors.Is(err, ErrPrecondition) {
		t.Fatal("missing exact recipient set accepted", err)
	}
	head, err := service.GetPasswordVault(ctx, owner)
	if err != nil || head.Generation != 2 {
		t.Fatal("failed rotation advanced vault", err)
	}
	rotation.GrantEnvelopes = []string{b64Vault(vaultGrantFixture(t, fixtureGrantClaims(team, rotation.OperationID, 2, 1, next, third), 11))}
	rotated, err := service.RotateVaultTeam(ctx, owner, team, rotation)
	if err != nil || rotated.KeyEpoch != 2 {
		t.Fatal("rotation failed", err)
	}
	head, err = service.GetPasswordVault(ctx, owner)
	if err != nil || head.Generation != 3 {
		t.Fatal("rotation omitted key custody", err)
	}
	if _, err := service.GetVaultScope(ctx, member, team, "team", team, ""); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatal("removed member read new scope", err)
	}
	if _, err := service.GrantVaultTeamMember(ctx, owner, team, grant); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatal("removed member regrant replay accepted", err)
	}
	inbox, err = service.ListVaultGrants(ctx, member)
	if err != nil || len(inbox) != 0 {
		t.Fatal("removed member received grant", err)
	}
	removedMember, ok := vaultMember(rotated, member)
	if !ok || removedMember.Active || removedMember.MembershipGeneration != 2 {
		t.Fatal("rekey repeated membership removal")
	}
	if _, err := service.RotateVaultTeam(ctx, owner, team, rotation); err != nil {
		t.Fatal("exact rotation retry failed", err)
	}
	rotation.RemoveAccountIDs = []string{}
	if _, err := service.RotateVaultTeam(ctx, owner, team, rotation); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("operation substitution accepted", err)
	}

	// Existing machine installations may initialize ENV custody for the first
	// time after installation generation one; machine-control auth binds that
	// exact installation, independently of ENV recipient generation numbering.
	laterMachine := "vslater_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,installation_generation) VALUES ($1,$2,$3,'later vault','linux','amd64','/vault-test','online','released',3)`, laterMachine, owner, "later_env_"+suffix); err != nil {
		t.Fatal(err)
	}
	laterRequest := VaultHostKeyRequest{OperationID: "later_host_" + suffix, InstallationGeneration: 3, HostKeyGeneration: 3, HostPublic: b64Vault(bytes.Repeat([]byte{75}, 32))}
	if later, err := service.RegisterVaultHostKey(ctx, owner, laterMachine, laterRequest); err != nil || later.InstallationGeneration != 3 || later.HostKeyGeneration != 3 {
		t.Fatal("later installation could not initialize ENV", err)
	}
	laterRequest.InstallationGeneration = 2
	if _, err := service.RegisterVaultHostKey(ctx, owner, laterMachine, laterRequest); !errors.Is(err, ErrVersionConflict) {
		t.Fatal("wrong installation registered ENV recipient", err)
	}

	hostPublic := bytes.Repeat([]byte{74}, 32)
	hostRequest := VaultHostKeyRequest{OperationID: "hostkey_" + suffix, InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: b64Vault(hostPublic)}
	binding, err := service.RegisterVaultHostKey(ctx, owner, machine, hostRequest)
	if err != nil || binding.RecipientAccount != owner {
		t.Fatal("authenticated host key registration failed", err)
	}
	// Personal writes use one account epoch and concurrent successors have one winner.
	personalRaw := vaultScopeFixture(t, fixtureScopeHeader(owner, "personal", owner, 3, 1, 1, make([]byte, 32)), 11)
	personal, err := service.PutVaultScope(ctx, owner, "personal", "personal", owner, "", VaultScopePut{OperationID: "personal_" + suffix, Envelope: b64Vault(personalRaw)})
	if err != nil {
		t.Fatal(err)
	}
	pd := sha256.Sum256(personalRaw)
	successor := vaultScopeFixture(t, fixtureScopeHeader(owner, "personal", owner, 3, 1, 2, pd[:]), 11)
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, op := range []string{"race_a_", "race_b_"} {
		go func(op string) {
			<-start
			_, err := service.PutVaultScope(ctx, owner, "personal", "personal", owner, "", VaultScopePut{OperationID: op + suffix, Envelope: b64Vault(successor)})
			results <- err
		}(op)
	}
	close(start)
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, ErrVersionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal("concurrent scope CAS failed")
	}
	inventory, err := service.GetVaultPersonalScopes(ctx, owner)
	if err != nil || inventory.KeyEpoch != 1 || len(inventory.Scopes) != 1 {
		t.Fatal("personal inventory invalid", err)
	}
	personal = inventory.Scopes[0]
	// Explicit total loss starts a fresh writer, sharing identity and personal epoch.
	thirdDigest := sha256.Sum256(thirdRaw)
	resetRaw := passwordVaultFixtureWith(t, "https://control.example", owner, 4, thirdDigest[:], vaultFixtureOptions{writerSeedByte: 12, passwordEpoch: 2, recoveryEpoch: 2})
	var resetEnvelope passwordVaultEnvelope
	var resetBody passwordVaultBody
	var resetHeader passwordVaultHeader
	_ = strictDecoding.Unmarshal(resetRaw, &resetEnvelope)
	_ = strictDecoding.Unmarshal(resetEnvelope.UnsignedBody, &resetBody)
	_ = strictDecoding.Unmarshal(resetBody.Header, &resetHeader)
	resetHeader.SharingPublic = bytes.Repeat([]byte{73}, 32)
	resetBody.Header, _ = EncodeCanonical(resetHeader)
	resetEnvelope.UnsignedBody, _ = EncodeCanonical(resetBody)
	message, _ := EncodeCanonical([]any{"paperboat.environment.vault-signature", uint64(1), resetEnvelope.UnsignedBody})
	resetEnvelope.Signature = ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{12}, 32)), message)
	resetRaw, _ = EncodeCanonical(resetEnvelope)
	successorDigest := sha256.Sum256(successor)
	replacement := vaultScopeFixture(t, fixtureScopeHeader(owner, "personal", owner, 4, 2, personal.Revision+1, successorDigest[:]), 12)
	reset := VaultPersonalReset{OperationID: "reset_" + suffix, ExpectedVaultDocumentID: third.Head.DocumentID, VaultEnvelope: b64Vault(resetRaw), ScopeEnvelopes: []string{}, ConfirmTotalLoss: true}
	if _, err := service.ResetPersonalVault(ctx, owner, reset); !errors.Is(err, ErrPrecondition) {
		t.Fatal("incomplete reset accepted", err)
	}
	reset.ScopeEnvelopes = []string{b64Vault(replacement)}
	if _, err := service.ResetPersonalVault(ctx, owner, reset); err != nil {
		t.Fatal(err)
	}
	inventory, err = service.GetVaultPersonalScopes(ctx, owner)
	if err != nil || inventory.KeyEpoch != 2 || inventory.Scopes[0].KeyEpoch != 2 {
		t.Fatal("reset failed to advance personal epoch", err)
	}
	if _, err := service.ResetPersonalVault(ctx, owner, reset); err != nil {
		t.Fatal("reset exact retry failed", err)
	}
	currentTeam, err := service.GetVaultTeam(ctx, owner, team)
	if err != nil || currentTeam.Scope.WriterPublic != rotated.Scope.WriterPublic || currentTeam.Scope.Envelope != rotated.Scope.Envelope {
		t.Fatal("reset owner cannot read the signed predecessor required for total-loss recovery", err)
	}
	var storedSigner []byte
	if err := store.SQL().QueryRowContext(ctx, `SELECT writer_public FROM paperboat.environment_vault_scopes WHERE owner_kind='team' AND owner_id=$1`, team).Scan(&storedSigner); err != nil || b64Vault(storedSigner) != rotated.Scope.WriterPublic {
		t.Fatal("reset replaced historical committed team signer", err)
	}
	if _, err := service.GetVaultScope(ctx, owner, team, "team", team, ""); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatal("reset silently restored team keys", err)
	}

	if _, err := service.CreateVaultTeam(ctx, owner, create); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatal("reset owner initialization replay exposed ciphertext", err)
	}
	resetMetadata, err := ParsePasswordVaultMetadata(resetRaw, "https://control.example", owner)
	if err != nil {
		t.Fatal(err)
	}
	overrideHeader := fixtureScopeHeader(owner, "personal", owner, 4, 2, 1, make([]byte, 32))
	overrideHeader.MachineID = machine
	if _, err := service.PutVaultScope(ctx, owner, "personal", "personal", owner, machine, VaultScopePut{OperationID: "override_" + suffix, Envelope: b64Vault(vaultScopeFixture(t, overrideHeader, 12))}); err != nil {
		t.Fatal(err)
	}
	// Staged personal rotation retains usable committed ciphertext until all scopes
	// and the replacement vault can commit together.
	rotationInventory, err := service.GetVaultPersonalScopes(ctx, owner)
	if err != nil || rotationInventory.RotationRequired || len(rotationInventory.Scopes) != 2 {
		t.Fatal("rotation inventory invalid", err)
	}
	resetDigest := sha256.Sum256(resetRaw)
	resetHeader.Generation = 5
	resetHeader.Previous = resetDigest[:]
	resetBody.Header, _ = EncodeCanonical(resetHeader)
	resetBody.Ciphertext = bytes.Repeat([]byte{19}, 17)
	resetEnvelope.UnsignedBody, _ = EncodeCanonical(resetBody)
	message, _ = EncodeCanonical([]any{"paperboat.environment.vault-signature", uint64(1), resetEnvelope.UnsignedBody})
	resetEnvelope.Signature = ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{12}, 32)), message)
	fifthRaw, _ := EncodeCanonical(resetEnvelope)
	rotatePersonal := VaultPersonalRotate{OperationID: "personalrotate_" + suffix, ExpectedVaultDocumentID: resetMetadata.Head.DocumentID, VaultEnvelope: b64Vault(fifthRaw), ScopeDocuments: []VaultScopeDocument{}}
	for _, old := range rotationInventory.Scopes {
		oldDigest, err := hex.DecodeString(strings.TrimPrefix(old.DocumentID, "sha256:"))
		if err != nil {
			t.Fatal(err)
		}
		header := fixtureScopeHeader(owner, "personal", owner, 5, 3, old.Revision+1, oldDigest)
		header.MachineID = old.MachineID
		header.WorkspaceID = old.WorkspaceID
		encoded := b64Vault(vaultScopeFixture(t, header, 12))
		// Total-loss reset rebuilt the empty record journal at sequence 1.
		// Preserve the actual inventory sequence when explicitly rotating it.
		records, err := service.ListVaultRecords(ctx, owner, VaultLayerCoordinate{WorkspaceID: old.WorkspaceID, OwnerKind: "personal", OwnerID: owner, MachineID: old.MachineID}, VaultRecordsQuery{Limit: 20})
		if err != nil || len(records.Records) != 0 {
			t.Fatalf("rotation record inventory count=%d err=%v", len(records.Records), err)
		}
		stage := VaultPersonalScopeStage{WorkspaceID: old.WorkspaceID, ExpectedVaultDocumentID: rotatePersonal.ExpectedVaultDocumentID, MachineID: old.MachineID, Envelope: encoded, Records: VaultRecordReplacement{ExpectedSequence: records.Sequence, DocumentIDs: []string{}, Envelopes: []string{}}}
		document, err := service.StagePersonalVaultScope(ctx, owner, rotatePersonal.OperationID, stage)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.StagePersonalVaultScope(ctx, owner, rotatePersonal.OperationID, stage); err != nil {
			t.Fatal("stage retry failed", err)
		}
		rotatePersonal.ScopeDocuments = append(rotatePersonal.ScopeDocuments, document)
	}
	partial := rotatePersonal
	partial.ScopeDocuments = partial.ScopeDocuments[:1]
	if _, err := service.RotatePersonalVault(ctx, owner, partial); !errors.Is(err, ErrPrecondition) {
		t.Fatal("incomplete staged rotation accepted", err)
	}
	rotatedHead, err := service.RotatePersonalVault(ctx, owner, rotatePersonal)
	if err != nil || rotatedHead.Generation != 5 {
		t.Fatal("staged rotation failed", err)
	}
	if _, err := service.RotatePersonalVault(ctx, owner, rotatePersonal); err != nil {
		t.Fatal("committed staged retry failed", err)
	}
	rotationInventory, err = service.GetVaultPersonalScopes(ctx, owner)
	if err != nil || rotationInventory.RotationRequired || rotationInventory.KeyEpoch != 3 {
		t.Fatal("rotation did not release personal fence", err)
	}
	var stagedCount int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.environment_vault_personal_rotation_scopes WHERE account_id=$1`, owner).Scan(&stagedCount); err != nil || stagedCount != 0 {
		t.Fatal("rotation leaked staging", err)
	}

	// Recovery reads preserve the historical signer but confer no normal scope authority.
	recoveredTeam, err := service.GetVaultTeam(ctx, owner, team)
	if err != nil || recoveredTeam.Scope.Envelope != rotated.Scope.Envelope {
		t.Fatal("missing recovery predecessor", err)
	}
	if _, err := service.GetVaultTeam(ctx, member, team); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatal("removed member obtained recovery predecessor", err)
	}
	previousScope, err := hex.DecodeString(strings.TrimPrefix(recoveredTeam.Scope.DocumentID, "sha256:"))
	if err != nil {
		t.Fatal(err)
	}
	fifthMetadata, err := ParsePasswordVaultMetadata(fifthRaw, "https://control.example", owner)
	if err != nil {
		t.Fatal(err)
	}
	fifthDigest := sha256.Sum256(fifthRaw)
	resetHeader.Generation = 6
	resetHeader.Previous = fifthDigest[:]
	resetBody.Header, _ = EncodeCanonical(resetHeader)
	resetEnvelope.UnsignedBody, _ = EncodeCanonical(resetBody)
	message, _ = EncodeCanonical([]any{"paperboat.environment.vault-signature", uint64(1), resetEnvelope.UnsignedBody})
	resetEnvelope.Signature = ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{12}, 32)), message)
	sixthRaw, _ := EncodeCanonical(resetEnvelope)
	sixthMetadata, err := ParsePasswordVaultMetadata(sixthRaw, "https://control.example", owner)
	if err != nil {
		t.Fatal(err)
	}
	recoverRequest := VaultTeamRotate{OperationID: "recover_team_" + suffix, ExpectedTeamGeneration: recoveredTeam.Generation, ConfirmTotalLoss: true, RemoveAccountIDs: []string{}, ScopeEnvelope: b64Vault(vaultScopeFixture(t, fixtureScopeHeader(team, "team", owner, 5, recoveredTeam.KeyEpoch+1, recoveredTeam.Scope.Revision+1, previousScope), 12)), VaultEnvelope: b64Vault(sixthRaw)}
	// GetVaultTeam exposes the committed predecessor journal sequence to
	// an Owner performing total-loss recovery even after losing key custody.
	recoverRequest.Records = VaultRecordReplacement{ExpectedSequence: recoveredTeam.RecordSequence, DocumentIDs: []string{}, Envelopes: []string{}}
	recoverRequest.GrantEnvelopes = []string{b64Vault(vaultGrantFixture(t, fixtureGrantClaims(team, recoverRequest.OperationID, recoveredTeam.KeyEpoch+1, 1, fifthMetadata, sixthMetadata), 12))}
	recovered, err := service.RotateVaultTeam(ctx, owner, team, recoverRequest)
	if err != nil || recovered.KeyEpoch != recoveredTeam.KeyEpoch+1 {
		t.Fatal("owner total-loss Team recovery failed", err)
	}
	if _, err := service.GetVaultScope(ctx, owner, team, "team", team, ""); err != nil {
		t.Fatal("recovered owner scope unavailable", err)
	}

}
