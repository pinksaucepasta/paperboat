package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Deleting a machine deletes its terminal sessions; per session row the peer_policy trigger reads active access
// sessions by terminal_session_id and the cascade deletes its operations. Both must use an index (367, 368): a seq
// scan per row made one machine's final delete take over 25 s under the shared 443 names lock.
func TestMachineDeleteTerminalSessionLookupsUseIndexesOnPostgres(t *testing.T) {
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
	tx, err := store.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT 1 FROM paperboat.user_machine_access_sessions WHERE terminal_session_id = 'x' AND state = 'active'`,
		`SELECT 1 FROM paperboat.user_machine_terminal_session_operations WHERE terminal_session_id = 'x'`,
	} {
		rows, err := tx.QueryContext(ctx, "EXPLAIN "+query)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(plan.String(), "Seq Scan") {
			t.Fatalf("%s\nscans the table:\n%s", query, plan.String())
		}
	}
}
