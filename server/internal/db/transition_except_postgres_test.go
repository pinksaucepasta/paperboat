package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Every statement trigger that compares a table's old and new transition rows must stay linear: matched by key as a
// join, the planner ran them as nested loops (transition tables have no statistics), ~7 s for 5k rows on prod
// 2026-11-19. Each function runs here as the UPDATE trigger of a same-shaped scratch copy of its table (constraints
// and foreign keys left out, so 20,000 rows need no fixtures), with the transition table names it reads; a 20,000-row
// UPDATE, run directly, from inside a PL/pgSQL block and after single-row updates on the same connection, must take under a second. The real tables' triggers are covered by their own suites.
func TestTransitionTableTriggersAreLinearOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		table, function, old, new string
		columns, values, set      string
	}{
		{"team_resource_bindings", "publish_binding_members", "old_bindings", "new_bindings",
			"team_id,resource_kind,resource_id,active,generation", "'t'||g%50,'machine','r'||g,true,1", "generation=generation+1"},
		{"control_config_credentials", "publish_config_credential_revocations", "old_rows", "new_rows",
			"jti,machine_id", "'j'||g,'m'||g%100", "revoked_at=clock_timestamp()"},
		{"machine_service_snapshots", "publish_machine_services_changed", "old_snapshots", "new_snapshots",
			"machine_id,services", "'m'||g,'[]'::jsonb", "services=jsonb_build_array(random())"},
		{"endpoint_edge_assignments", "publish_private_routes_assignments", "old_rows", "new_rows",
			"route_id,edge_node_id,endpoint_id,desired,observed_ready,generation,carrier_session_id,edge_process_epoch", "'r'||g,'e','ep'||g%100,'active',false,1,'c','p'", "generation=generation+1"},
		{"runtime_carrier_attachments", "publish_private_routes_carriers", "old_rows", "new_rows",
			"route_id,account_id,session_id,edge_process_epoch,admission", "'r'||g,'a'||g%100,'s'||g,'p','{}'::jsonb", "session_id=session_id||'x'"},
		{"endpoints", "publish_private_routes_endpoints", "old_rows", "new_rows",
			"id,account_id,desired_state,access_mode", "'ep'||g,'a'||g%100,'active','private'", "desired_state=CASE WHEN desired_state='active' THEN 'removed' ELSE 'active' END"},
		{"preview_leases", "publish_private_routes_leases", "old_rows", "new_rows",
			"id,account_id,terminal_state,access_mode", "'pl'||g,'a'||g%100,'active','private'", "terminal_state=CASE WHEN terminal_state='active' THEN 'expired' ELSE 'active' END"},
		{"team_members", "publish_team_members_recording_policy", "old_members", "new_members",
			"team_id,account_id,active,role", "'t'||g%50,'a'||g%2000,true,'member'", "active=NOT active"},
		{"team_members", "publish_team_memberships", "old_members", "new_members",
			"team_id,account_id,active,role", "'t'||g%50,'a'||g%2000,true,'member'", "active=NOT active"},
		{"teams", "publish_team_suspension_members", "old_teams", "new_teams",
			"team_id,suspended", "'t'||g,false", "suspended=NOT suspended"},
	}
	for _, c := range cases {
		t.Run(c.function, func(t *testing.T) {
			// One connection: the scratch table is a temporary table of this session.
			conn, err := store.SQL().Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for _, q := range []string{
				`CREATE TEMP TABLE probe (LIKE paperboat.` + c.table + `)`,
				`DO $$ DECLARE c text; BEGIN FOR c IN SELECT attname FROM pg_attribute WHERE attrelid='probe'::regclass AND attnum>0 AND NOT attisdropped AND attnotnull LOOP EXECUTE format('ALTER TABLE probe ALTER COLUMN %I DROP NOT NULL', c); END LOOP; END $$`,
				`INSERT INTO probe(` + c.columns + `) SELECT ` + c.values + ` FROM generate_series(1,20000) g`,
				`CREATE TRIGGER probe_update AFTER UPDATE ON probe REFERENCING OLD TABLE AS ` + c.old + ` NEW TABLE AS ` + c.new +
					` FOR EACH STATEMENT EXECUTE FUNCTION paperboat.` + c.function + `()`,
			} {
				if _, err := conn.ExecContext(ctx, q); err != nil {
					t.Fatalf("%v\n%s", err, q)
				}
			}
			defer func() { _, _ = conn.ExecContext(context.Background(), `DROP TABLE IF EXISTS probe`) }()
			// Direct, from inside a PL/pgSQL block as a trigger cascade runs it, and after single-row updates on the same
			// connection: PL/pgSQL then reuses a generic plan made for one-row transition tables (how prod hit it: pooled
			// connections run small updates, then one bulk change).
			for _, mode := range []struct{ name, sql string }{
				{"direct", `UPDATE probe SET ` + c.set},
				{"nested", `DO $$ BEGIN UPDATE probe SET ` + c.set + `; END $$`},
				{"after small updates", `UPDATE probe SET ` + c.set},
			} {
				if mode.name == "after small updates" {
					for range 8 {
						if _, err := conn.ExecContext(ctx, `UPDATE probe SET `+c.set+` WHERE ctid=(SELECT ctid FROM probe LIMIT 1)`); err != nil {
							t.Fatal(err)
						}
					}
				}
				start := time.Now()
				if _, err := conn.ExecContext(ctx, mode.sql); err != nil {
					t.Fatal(err)
				}
				took := time.Since(start)
				if took > time.Second {
					t.Errorf("a 20,000-row UPDATE (%s) of %s through %s took %v, want under 1 s", mode.name, c.table, c.function, took)
				}
				t.Logf("%s through %s, %s: %v", c.table, c.function, mode.name, took)
			}
		})
	}
}
