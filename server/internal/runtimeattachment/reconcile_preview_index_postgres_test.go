package runtimeattachment

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
)

func TestReconcileFindsLivePreviewsByOwnerWithoutScanningRetainedLeasesPostgres(t *testing.T) {
	f := newAgentFixture(t, "reconcile_preview_index")
	// The other indexes on active leases cover every machine's; with other machines' active leases present, only the
	// per-owner index keeps this lookup independent of how many previews the whole fleet has live.
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM preview_leases WHERE owner_machine_id LIKE $1||'_o%'`, `DELETE FROM user_machines WHERE id LIKE $1||'_o%'`, `DELETE FROM control_environments WHERE id LIKE $1||'_o%'`} {
			_, _ = f.database.SQL().ExecContext(f.ctx, q, f.prefix)
		}
	})
	for i := range 20 {
		machine, env := fmt.Sprintf("%s_o%02d", f.prefix, i), fmt.Sprintf("%s_o%02d", f.prefix, i)
		f.exec(t, `INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, env, f.account)
		f.exec(t, `INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation,worker_generation,configured_capabilities) VALUES ($1,$2,$3,replace($1,'_','-'),'linux','amd64','/workspace','online','occupied',translate(rtrim(encode(sha256(convert_to($1,'UTF8')),'base64'),'='),'+/','-_'),1,1,ARRAY['terminal_host']::text[])`, machine, f.account, env)
		f.exec(t, `INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline)
SELECT $1||'_p'||g,'https://'||replace($1,'_','-')||'-'||g||'.preview.example.test',$2,$2,$1,'owner_session_index','foreground','http','127.0.0.1:3000','team',now()+interval '10 minutes' FROM generate_series(1,15) g`, machine, f.account)
	}
	f.exec(t, `ANALYZE preview_leases`)
	tx, err := f.database.SQL().BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// A tiny fixture is legitimately cheaper to scan; discourage that so the plan shows which index exists.
	if _, err := tx.ExecContext(f.ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(f.ctx, "EXPLAIN (COSTS OFF) "+endpoints.LivePreviewsByOwner, f.machine)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "preview_leases_active_by_owner") {
		t.Fatalf("live preview lookup does not use the active-by-owner index:\n%s", plan.String())
	}
}
