package usermachines

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// Right after a burst of sessions, before autoanalyze has run, the statistics still describe the old tables; a retention
// query whose plan depended on them ran into the 30 s statement timeout (3 of 8 runs, rc-fd 6376eefeb). Each retention
// batch must take well under a second with statistics taken while the tables were nearly empty, then 40,000 sessions
// inserted: 20,000 expired on an ended machine, 20,000 idle expired on a live one, in a 300-machine account.
func TestRetentionBatchesArePlanStableWithStaleStatisticsOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	x := fmt.Sprint(time.Now().UnixNano())
	user, cli := "plan_user_"+x, "plan_cli_"+x
	ended, live := "plan_ended_"+x, "plan_live_"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM paperboat.native_usage_authorities WHERE actor_account_id=$1`,
			`DELETE FROM paperboat.user_machine_access_sessions WHERE user_id=$1`,
			`DELETE FROM paperboat.cli_client_sessions WHERE user_id=$1`,
			`DELETE FROM paperboat.user_machines WHERE user_id=$1`,
			`DELETE FROM paperboat.users WHERE id=$1`,
		} {
			_, _ = store.SQL().ExecContext(bg, q, user)
		}
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation,revoked_at,deleted_at)
VALUES($1,$3,'env_'||$1,'linux','amd64','/w','disconnected','released',false,1,now(),now()),($2,$3,'env_'||$2,'linux','amd64','/w','online','occupied',true,1,NULL,NULL)`, ended, live, user)
	// The rest of a 300-machine account (the plan joins the account's machines).
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
SELECT 'plan_m_'||g||'_'||$2,$1,'env_plan_m_'||g||'_'||$2,'linux','amd64','/w','online','occupied',true,1 FROM generate_series(1,298) g`, user, x)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,'client','plan','desktop','test',ARRAY['machines:connect'],'active',now(),now())`, cli, user)
	// Statistics of the nearly empty tables, as before a burst.
	exec(`ANALYZE paperboat.user_machine_access_sessions`)
	exec(`ANALYZE paperboat.user_machines`)
	exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,state,helper_terminal_session_id,capabilities,operation_id)
SELECT 'control_plane_signed','plan_'||m.id||'_'||g,m.id,$1,'env_'||m.id,$2,'',now()-interval '2 days','active','h_'||m.id||'_'||g,ARRAY['exec'],'op'
FROM generate_series(1,20000) g, paperboat.user_machines m WHERE m.id IN ($3,$4)`, user, cli, ended, live)

	service := New(store, nil, Policy{ExpiredAccessSessionRetention: 7 * 24 * time.Hour}, nil)
	batch := func(name string, run func() (int64, error)) {
		t.Helper()
		start := time.Now()
		n, err := run()
		took := time.Since(start)
		if err != nil {
			t.Fatalf("%s: %v (after %v)", name, err, took)
		}
		if n != retireBatchSize {
			t.Fatalf("%s retired %d sessions, want a full batch of %d", name, n, retireBatchSize)
		}
		if took > time.Second {
			t.Errorf("%s batch took %v with stale statistics, want under 1 s", name, took)
		}
		t.Logf("%s: %d sessions in %v", name, n, took)
	}
	batch("ended machines", func() (int64, error) {
		return service.db.Queries().RetireAccessSessionsOfEndedMachines(ctx, retireBatchSize)
	})
	now := time.Now().UTC()
	batch("idle expired", func() (int64, error) {
		return service.db.Queries().RetireExpiredMachineAccessSessions(ctx, dbsqlc.RetireExpiredMachineAccessSessionsParams{
			IdleBefore: now.Add(-13 * time.Hour), ExpiredBefore: now.Add(-7 * 24 * time.Hour), BatchSize: retireBatchSize})
	})
}
