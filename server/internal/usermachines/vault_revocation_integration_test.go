package usermachines

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

func TestExplicitMachineRevocationFencesVaultAndTeams(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, deletion := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete_%t", deletion), func(t *testing.T) {
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			account, machine, team := "vraccount_"+suffix, "vrmachine_"+suffix, "vrteam_"+suffix
			// Successful revocation creates immutable audit history. Retain its
			// unique fixture account until the isolated test database is dropped.

			if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, account); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state) VALUES ($1,$2,$3,'linux','amd64','/revoke-test','online','released')`, machine, account, "env_"+suffix); err != nil {
				t.Fatal(err)
			}
			// The revocation hook only observes custody existence; no cryptographic claim
			// is made about this opaque storage fixture.
			if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.environment_password_vaults(account_id,generation,document_id,envelope) VALUES($1,1,'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','opaque-test-envelope')`, account); err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`} {
				if _, err := store.SQL().ExecContext(ctx, q, team, account); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.environment_vault_teams(team_id,key_epoch) SELECT $1,1 WHERE $2<>''`, team, account); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.environment_vault_team_members(team_id,account_id,grant_epoch) VALUES($1,$2,1)`, team, account); err != nil {
				t.Fatal(err)
			}
			// Source fences are independent of destination host registrations and
			// cover private account/device scopes in every workspace.
			public := bytes.Repeat([]byte{19}, 32)
			for _, coordinate := range []struct{ workspace, kind, owner, sourceMachine string }{
				{"personal", "personal", account, ""}, {team, "personal", account, ""}, {team, "personal", account, machine}, {team, "team", team, ""}, {"personal", "personal", "unrelated_" + suffix, ""},
			} {
				if _, err := store.SQL().ExecContext(ctx, `INSERT INTO environment_vault_scopes(workspace_id,owner_kind,owner_id,machine_id,key_epoch,revision,document_id,envelope,writer_public) VALUES($1,$2,$3,$4,1,1,'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',$5,$6)`, coordinate.workspace, coordinate.kind, coordinate.owner, coordinate.sourceMachine, []byte{1}, public); err != nil {
					t.Fatal(err)
				}
				if _, err := store.SQL().ExecContext(ctx, `INSERT INTO environment_vault_layer_fences(workspace_id,owner_kind,owner_id,source_machine_id,generation) VALUES($1,$2,$3,$4,1)`, coordinate.workspace, coordinate.kind, coordinate.owner, coordinate.sourceMachine); err != nil {
					t.Fatal(err)
				}
			}
			// The recipient can hold shared ciphertext even without a Personal Team
			// key grant. That custody must still fence Team delivery on revocation.
			if !deletion {
				if _, err := store.SQL().ExecContext(ctx, `DELETE FROM environment_vault_team_members WHERE team_id=$1 AND account_id=$2`, team, account); err != nil {
					t.Fatal(err)
				}
				if _, err := store.SQL().ExecContext(ctx, `INSERT INTO environment_vault_hosts(machine_id,account_id,installation_generation,host_key_generation,host_public) VALUES($1,$2,1,1,$3)`, machine, account, public); err != nil {
					t.Fatal(err)
				}
				if _, err := store.SQL().ExecContext(ctx, `INSERT INTO environment_vault_layers(machine_id,workspace_id,owner_kind,owner_id,source_machine_id,delivery_generation,document_id,installation_generation,host_key_generation,applied_fence_generation,writer_account,writer_public,envelope) VALUES($1,$2,'team',$2,'',1,'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',1,1,1,$3,$4,$5)`, machine, team, account, public, []byte{1}); err != nil {
					t.Fatal(err)
				}
			}
			service := New(store, audit.NewWriter(store), Policy{}, testSeatAuthorizer{})
			if err := service.Disconnect(ctx, "other_account", machine); err == nil {
				t.Fatal("cross-account disconnect accepted")
			}
			var count int
			if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.environment_vault_personal_epochs WHERE account_id=$1`, account).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed revocation changed vault state", err)
			}
			var err error
			if deletion {
				err = service.Delete(ctx, account, machine)
			} else {
				err = service.Disconnect(ctx, account, machine)
			}
			if err != nil {
				t.Fatal(err)
			}
			var personalRequired, teamRequired bool
			if err := store.SQL().QueryRowContext(ctx, `SELECT COALESCE((SELECT rotation_required FROM paperboat.environment_vault_personal_epochs WHERE account_id=$1),false)`, account).Scan(&personalRequired); err != nil {
				t.Fatal(err)
			}
			if err := store.SQL().QueryRowContext(ctx, `SELECT rotation_required FROM paperboat.environment_vault_teams WHERE team_id=$1`, team).Scan(&teamRequired); err != nil {
				t.Fatal(err)
			}
			if personalRequired || teamRequired {
				t.Fatal("machine removal must fence delivery without requiring key rotation")
			}
			// Only the sources the removed machine could read are fenced: the one it
			// sourced itself here, never the account's other private sources.
			var ownCount, sourcedByMachine int
			if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE generation=2),count(*) FILTER (WHERE generation=2 AND source_machine_id=$2) FROM environment_vault_layer_fences WHERE owner_kind='personal' AND owner_id=$1`, account, machine).Scan(&ownCount, &sourcedByMachine); err != nil || ownCount != 1 || sourcedByMachine != 1 {
				t.Fatalf("private source fences advanced=%d sourced by the machine=%d err=%v, want only the machine's own", ownCount, sourcedByMachine, err)
			}
			var teamFence, unrelatedFence uint64
			wantTeamFence := uint64(2) // a machine holding a team layer fences that team
			if deletion {
				wantTeamFence = 1 // a deleted machine that never received a layer leaves the team alone
			}
			if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM environment_vault_layer_fences WHERE owner_kind='team' AND owner_id=$1`, team).Scan(&teamFence); err != nil || teamFence != wantTeamFence {
				t.Fatalf("shared source fence=%d want %d err=%v", teamFence, wantTeamFence, err)
			}
			if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM environment_vault_layer_fences WHERE owner_kind='personal' AND owner_id=$1`, "unrelated_"+suffix).Scan(&unrelatedFence); err != nil || unrelatedFence != 1 {
				t.Fatalf("unrelated source fence changed=%d err=%v", unrelatedFence, err)
			}

		})
	}
}

func TestMemberWithoutPersonalVaultRemovalFencesSharedLayer(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, member, team := "vlro_"+suffix, "vlrm_"+suffix, "vlrt_"+suffix
	// The isolated test DB owns these unique fixtures, consistent with the
	// existing audit-preserving revocation fixtures above.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active'),($2,$2,$2||'@invalid.test','active')`, owner, member)
	exec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	for _, account := range []string{owner, member} {
		exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, account)
	}
	exec(`INSERT INTO environment_vault_teams(team_id,key_epoch) VALUES($1,1)`, team)
	exec(`INSERT INTO environment_vault_scopes(workspace_id,owner_kind,owner_id,machine_id,key_epoch,revision,document_id,envelope,writer_public) VALUES($1,'team',$1,'',1,1,'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',$2,$3)`, team, []byte{1}, bytes.Repeat([]byte{19}, 32))
	exec(`INSERT INTO environment_vault_layer_fences(workspace_id,owner_kind,owner_id,source_machine_id,generation) VALUES($1,'team',$1,'',1)`, team)
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error { return teams.RemoveMemberTx(ctx, tx, team, member) }); err != nil {
		t.Fatal(err)
	}
	var required, active bool
	var fence uint64
	if err := store.SQL().QueryRowContext(ctx, `SELECT t.rotation_required,f.generation,m.active FROM environment_vault_teams t JOIN environment_vault_layer_fences f ON f.owner_kind='team' AND f.owner_id=t.team_id JOIN team_members m ON m.team_id=t.team_id AND m.account_id=$2 WHERE t.team_id=$1`, team, member).Scan(&required, &fence, &active); err != nil {
		t.Fatal(err)
	}
	if !required || fence != 2 || active {
		t.Fatal("keyless member removal did not atomically revoke and fence Team ENV")
	}
}
