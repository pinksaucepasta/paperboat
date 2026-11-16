package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// The stored_files topic moves only when a write changes status, stored_uploaded or stored_prefix of a ready request
// (330), never on a write that leaves them as they were; and the certificate holders' user_id index exists (329).
func TestStoredFilesTopicMovesOnlyOnRealChangesOnPostgres(t *testing.T) {
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
	conn, err := store.SQL().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s := fmt.Sprint(time.Now().UnixNano())
	user, machine := "it_sf_user_"+s, "it_sf_machine_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online)
VALUES($1,$2,$1,'linux','amd64','/w','online','occupied',true)`, machine, user)
	// The real trigger function on a table with the columns it reads (an inbox request needs a whole team fixture).
	exec(`CREATE TEMP TABLE stored_files_probe (status text, stored_uploaded boolean, stored_prefix text, recipient_account text)`)
	exec(`CREATE TRIGGER stored_files_probe AFTER INSERT OR UPDATE ON pg_temp.stored_files_probe FOR EACH ROW EXECUTE FUNCTION paperboat.publish_stored_files()`)
	generation := func() (g int64) {
		t.Helper()
		if err := conn.QueryRowContext(ctx, `SELECT coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='stored_files'),0)`, machine).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	for _, step := range []struct {
		name, query string
		bump        bool
	}{
		{"a ready request arrives", `INSERT INTO pg_temp.stored_files_probe VALUES('approved',true,'prefix-1',$1)`, true},
		{"the same values written again", `UPDATE pg_temp.stored_files_probe SET status='approved',stored_uploaded=true WHERE recipient_account=$1`, false},
		{"an unrelated column changes", `UPDATE pg_temp.stored_files_probe SET recipient_account=recipient_account WHERE recipient_account=$1`, false},
		{"the stored prefix changes", `UPDATE pg_temp.stored_files_probe SET stored_prefix='prefix-2' WHERE recipient_account=$1`, true},
	} {
		before := generation()
		exec(step.query, user)
		if bumped := generation() != before; bumped != step.bump {
			t.Fatalf("%s: topic moved=%v, want %v", step.name, bumped, step.bump)
		}
	}
	var indexed bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='paperboat' AND tablename='managed_ssh_certificate_holders' AND indexdef LIKE '%(user_id)%')`).Scan(&indexed); err != nil || !indexed {
		t.Fatalf("managed_ssh_certificate_holders has no user_id index: %v", err)
	}
}
