package peersessions

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A configuration request records which of the endpoint's expired access sessions it still names as admitted (an open
// stream), so the usermachines sweeper keeps exactly those; a session it does not own, one that has not expired and
// one recorded moments ago are left alone.
func TestConfigurationRecordsAdmittedExpiredSessionsOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	x := fmt.Sprint(time.Now().UnixNano())
	user, other, machine, cli := "seen_user_"+x, "seen_other_"+x, "seen_m_"+x, "seen_cli_"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, user, other)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,'env_'||$1,'linux','amd64','/w','online','occupied',true,1)`, machine, user)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,'client','seen','desktop','test',ARRAY['machines:connect'],'active',now(),now()),($3,$4,'client','seen','desktop','test',ARRAY['machines:connect'],'active',now(),now())`, cli, user, cli+"_other", other)
	add := func(id, owner, client string, expires string, seen any) {
		exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,state,helper_terminal_session_id,capabilities,operation_id,admitted_seen_at)
VALUES('control_plane_signed',$1,$2,$3,'env_'||$2,$4,'',now()+$5::interval,'active','h_'||$1,ARRAY['exec'],'op',$6)`, id, machine, owner, client, expires, seen)
	}
	add("seen_expired_"+x, user, cli, "-1 day", nil)
	add("seen_live_"+x, user, cli, "1 hour", nil)
	add("seen_recent_"+x, user, cli, "-1 day", time.Now().Add(-time.Minute))
	add("seen_foreign_"+x, other, cli+"_other", "-1 day", nil)
	service, err := NewNetworkService(store, meshTestSigner{}, "https://issuer.example.test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ids := []string{"seen_expired_" + x, "seen_live_" + x, "seen_recent_" + x, "seen_foreign_" + x}
	if err := service.markAdmittedSessions(ctx, NetworkConfigRequest{UserID: user, EndpointID: cli}, ids, now); err != nil {
		t.Fatal(err)
	}
	seen := func(id string) sql.NullTime {
		var v sql.NullTime
		if err := store.SQL().QueryRowContext(ctx, `SELECT admitted_seen_at FROM paperboat.user_machine_access_sessions WHERE id=$1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := seen("seen_expired_" + x); !v.Valid || v.Time.Sub(now).Abs() > time.Second {
		t.Fatalf("the endpoint's expired admitted session was not recorded: %v", v)
	}
	if v := seen("seen_live_" + x); v.Valid {
		t.Fatal("an unexpired session was written")
	}
	if v := seen("seen_recent_" + x); !v.Valid || now.Sub(v.Time) < 30*time.Second {
		t.Fatalf("a session recorded a minute ago was written again: %v", v)
	}
	if v := seen("seen_foreign_" + x); v.Valid {
		t.Fatal("another account's session was written")
	}
}
