package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// GET /v1/tunnels runs a page and the total through manageable_tunnel_ids. At ~240 tunnels on one account it took 2-9 s
// on prod (2026-11-12); plans allow 1,000 tunnels per account, and a page plus its total stays well under a second.
func TestTunnelListOfAThousandTunnelsOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	actor := fmt.Sprintf("tl_scale_%d", time.Now().UnixNano())
	machine := actor + "_m"
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.tunnels WHERE account_id=$1`, actor)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE user_id=$1`, actor)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_environments WHERE owner_user_id=$1`, actor)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, actor)
	})
	const tunnels = 1000
	for _, statement := range []string{
		`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`,
		`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1||'_env',$1||'_env',$1)`,
		`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
 VALUES($1||'_m',$1,$1||'_env','scale','linux','amd64','/w','online','occupied',true,1)`,
		`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code,created_at,updated_at)
 SELECT $1||'_t'||g,$1,'scale-'||g,'active','public',1,'endpoint_'||gen_random_uuid(),'https://'||replace($1,'_','-')||'-'||g||'.tunnels.example.test',$1||'_m',$1,'pending',now()-g*interval '1 second',now() FROM generate_series(1,1000) g`,
	} {
		if _, err := store.SQL().ExecContext(ctx, statement, actor); err != nil {
			t.Fatalf("%v\n%s", err, statement)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `ANALYZE paperboat.tunnels`); err != nil {
		t.Fatal(err)
	}
	var best time.Duration
	for range 3 { // the best of three: one slow run on a shared test host is not the plan
		started := time.Now()
		page, err := store.Queries().ListPreviewTunnelsV1(ctx, dbsqlc.ListPreviewTunnelsV1Params{AccountID: actor, RowLimit: 50, MachineID: machine})
		if err != nil || len(page) != 50 {
			t.Fatalf("page = %d tunnels, %v; want 50", len(page), err)
		}
		total, err := store.Queries().CountPreviewTunnelsV1(ctx, dbsqlc.CountPreviewTunnelsV1Params{AccountID: actor, MachineID: machine})
		if err != nil || total != tunnels {
			t.Fatalf("total = %d, %v; want %d", total, err, tunnels)
		}
		if run := time.Since(started); best == 0 || run < best {
			best = run
		}
	}
	t.Logf("a page and the total of %d tunnels: %v", tunnels, best)
	if best > 500*time.Millisecond {
		t.Fatalf("a page and the total of %d tunnels took %v, want under 500ms", tunnels, best)
	}
}
