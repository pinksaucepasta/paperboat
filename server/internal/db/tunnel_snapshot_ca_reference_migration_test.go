package db

import (
	"context"
	"crypto/sha256"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pressly/goose/v3"
)

// 388: a live tunnel whose current snapshot still has the retired route key "ca_reference":null gets a new pending
// generation without it (generation bumped, hash recomputed); a deleted tunnel, a current snapshot without the key and
// the old generations stay as they were.
func TestTunnelSnapshotDropsRetiredCAReference(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_MIGRATION_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("empty isolated migration PostgreSQL DSN required")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	var exists bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.users') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("requires empty isolated database: exists=%v error=%v", exists, err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA paperboat`)
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err = goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 387); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('owner','owner','owner@example.test','active')`)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES('machine','owner','machine','linux','amd64','/workspace','online','occupied',true)`)
	const route = `{"ca_reference":null,"connect_timeout_ms":10000,"desired_state":"active","host_override":null,"id":"rte_%s","idle_timeout_ms":90000,"match_hostname":"%s.tunnels.example.test","match_type":"managed_exact","max_concurrent_streams":1024,"mtls_credential_reference":null,"name":"default","origin_address":"127.0.0.1:18099","origin_scheme":"http","path_prefix":null,"preserve_host":true,"protocol":"http","tls_server_name":null,"tls_verification":"not_applicable"}`
	tunnel := func(id string, deleted bool, ca bool) {
		host := strings.ReplaceAll(id, "_", "-") + ".tunnels.example.test"
		endpoint := "123e4567-e89b-42d3-a456-" + strings.Repeat(id[len(id)-1:], 12)
		body := strings.NewReplacer("rte_%s", "rte_"+id, "%s", strings.ReplaceAll(id, "_", "-")).Replace(route)
		if !ca {
			body = strings.Replace(body, `"ca_reference":null,`, "", 1)
		}
		snapshot := `{"access_mode":"public","desired_state":"active","expires_at":null,"generation":1,"kind":"tunnel_config_snapshot","name":"` + id + `","routes":[` + body + `],"schema":"paperboat.preview-tunnel/v1","stable_endpoint":"https://` + host + `","stable_endpoint_id":"endpoint_` + endpoint + `","tunnel_id":"` + id + `"}`
		sum := sha256.Sum256([]byte(snapshot))
		deletedAt, desired := "NULL", "active"
		if deleted {
			deletedAt, desired = "now()", "deleted"
		}
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,access_mode,generation,deleted_at,desired_state) VALUES($1,'owner',$1,'endpoint_`+endpoint+`','https://`+host+`','machine','owner','public',1,`+deletedAt+`,'`+desired+`')`, id)
		exec(`INSERT INTO paperboat.tunnel_config_generations(tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,created_at,activated_at,retained_until) VALUES($1,1,$2,$3,'active','owner',now(),now(),now()+interval '1 day')`, id, sum[:], []byte(snapshot))
	}
	tunnel("tunnel_a", false, true)
	tunnel("tunnel_b", true, true)
	tunnel("tunnel_c", false, false)
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 388); err != nil {
		t.Fatal(err)
	}
	var generation int64
	var state, snapshot string
	var previous int64
	var hashOK bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT t.generation, g.activation_state, g.previous_generation, convert_from(g.snapshot,'UTF8'), g.content_hash=sha256(g.snapshot)
FROM paperboat.tunnels t JOIN paperboat.tunnel_config_generations g ON g.tunnel_id=t.id AND g.generation=t.generation WHERE t.id='tunnel_a'`).Scan(&generation, &state, &previous, &snapshot, &hashOK); err != nil {
		t.Fatal(err)
	}
	if generation != 2 || state != "pending" || previous != 1 || !hashOK || strings.Contains(snapshot, "ca_reference") || !strings.Contains(snapshot, `"generation":2,`) {
		t.Fatalf("live tunnel: generation %d %s previous %d hash ok %v snapshot %s", generation, state, previous, hashOK, snapshot)
	}
	var untouched, oldKept int
	if err = store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.tunnels WHERE id IN ('tunnel_b','tunnel_c') AND generation=1),
 (SELECT count(*) FROM paperboat.tunnel_config_generations WHERE generation=1 AND activation_state='active' AND (tunnel_id IN ('tunnel_b','tunnel_c') OR position('"ca_reference"' in convert_from(snapshot,'UTF8'))>0 AND tunnel_id='tunnel_a'))`).Scan(&untouched, &oldKept); err != nil {
		t.Fatal(err)
	}
	if untouched != 2 || oldKept != 3 {
		t.Fatalf("untouched tunnels %d (want 2), kept generation-1 rows %d (want 3)", untouched, oldKept)
	}
}
