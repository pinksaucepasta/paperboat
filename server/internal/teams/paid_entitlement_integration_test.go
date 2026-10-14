package teams

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestPostgresTeamPaidEntitlementAdmissionExpiryAndRecovery(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}

	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	owner, member := "pe_owner_"+suffix, "pe_member_"+suffix
	team, machine, terminal := "pe-team-"+suffix, "pe_machine_"+suffix, "pe_terminal_"+suffix
	workspace, access := "pe_workspace_"+suffix, "pe_access_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, query, args...); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.users WHERE id IN($1,$2)`, owner, member)
	})

	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active'),($2,$2,$2||'@invalid.test','active')`, owner, member)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, team, owner, member)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,description,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,'paid-host','Paid team host','linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machine, owner, "env_"+suffix)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'env',$1,$2),($1,'machine',$3,$2)`, team, owner, machine)
	exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,'env',$1,'read',1,true)`, team, member)
	exec(`INSERT INTO paperboat.team_machine_grants(team_id,machine_id,audience,account_id,capabilities) VALUES($1,$2,'selected_member',$3,ARRAY['terminal'])`, team, machine, member)
	exec(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,idempotency_key,launch_cwd) VALUES($1,$2,$3,'shared',$1,'/workspace')`, terminal, machine, owner)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,terminal_process_generation) VALUES($1,'terminal_session',$2,$3,1)`, team, terminal, owner)
	exec(`INSERT INTO paperboat.team_terminal_session_grants(team_id,terminal_session_id,audience,account_id,role) VALUES($1,$2,'selected_member',$3,'viewer')`, team, terminal, member)

	service := NewService(store)
	if _, err = service.Authorize(ctx, member, team, "env", team, "read"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unsubscribed team authorized: %v", err)
	}
	var allowed bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.team_machine_capability_allowed($1,$2,$3,'terminal')`, member, team, machine).Scan(&allowed); err != nil || allowed {
		t.Fatalf("unsubscribed machine grant: allowed=%v err=%v", allowed, err)
	}
	var role *string
	if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.terminal_session_role($1,$2)`, member, terminal).Scan(&role); err != nil || role != nil {
		t.Fatalf("unsubscribed terminal grant: role=%v err=%v", role, err)
	}

	now := time.Now().UTC()
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,$3,$4)`, workspace, team, now.Add(-time.Hour), now.Add(time.Hour))
	if _, err = service.Authorize(ctx, member, team, "env", team, "read"); err != nil {
		t.Fatalf("paid team denied: %v", err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.team_machine_capability_allowed($1,$2,$3,'terminal')`, member, team, machine).Scan(&allowed); err != nil || !allowed {
		t.Fatalf("paid machine grant: allowed=%v err=%v", allowed, err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.terminal_session_role($1,$2)`, member, terminal).Scan(&role); err != nil || role == nil || *role != "viewer" {
		t.Fatalf("paid terminal grant: role=%v err=%v", role, err)
	}
	exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,expires_at,team_id,capabilities,operation_id) VALUES('external_issuer',$1,$2,$3,$4,$5,'https://invalid.test',$6,$7,$8,ARRAY['terminal'],$9)`, access, machine, member, "env_"+suffix, "cli_"+suffix, "jti_"+suffix, now.Add(time.Hour), team, terminal)

	// Expiring the paid period fences the team and revokes the existing access.
	exec(`UPDATE paperboat.billing_workspaces SET period_start=$2,period_end=$3 WHERE id=$1`, workspace, now.Add(-2*time.Hour), now.Add(-time.Hour))
	var state string
	if err = store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_access_sessions WHERE id=$1`, access).Scan(&state); err != nil || state != "revoked" {
		t.Fatalf("lapsed team session state=%q err=%v", state, err)
	}
	if _, err = service.Authorize(ctx, member, team, "env", team, "read"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expired team authorized: %v", err)
	}
	var generationBeforeExpiryWorker int64
	if err = store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, team).Scan(&generationBeforeExpiryWorker); err != nil {
		t.Fatal(err)
	}
	var expired int
	if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.expire_team_paid_entitlements(10)`).Scan(&expired); err != nil || expired < 1 {
		t.Fatalf("expiry reconciler changed=%d err=%v", expired, err)
	}
	var generationAfterExpiryWorker int64
	if err = store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, team).Scan(&generationAfterExpiryWorker); err != nil {
		t.Fatal(err)
	}
	if generationAfterExpiryWorker != generationBeforeExpiryWorker+1 {
		t.Fatalf("expiry worker team generation=%d, want %d", generationAfterExpiryWorker, generationBeforeExpiryWorker+1)
	}

	exec(`UPDATE paperboat.billing_workspaces SET state='active',period_start=$2,period_end=$3 WHERE id=$1`, workspace, now.Add(-time.Minute), now.Add(time.Hour))
	if _, err = service.Authorize(ctx, member, team, "env", team, "read"); err != nil {
		t.Fatalf("reactivated team denied fresh access: %v", err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.machine_capability_allowed($1,$2,'terminal')`, owner, machine).Scan(&allowed); err != nil || !allowed {
		t.Fatalf("personal owner affected by team billing: allowed=%v err=%v", allowed, err)
	}
}
