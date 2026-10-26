package metering

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A machine's runtime observation records the client version it runs (the count a release that changes
// client decoding waits on); an observation without a version keeps the last one.
func TestRuntimeObservationRecordsTheReporterVersionOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	user, machine := "rv_user_"+s, "rv_machine_"+s
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online) VALUES($2,$1,$2,'rv-'||right($2,6),'linux','amd64','/w','online','occupied',true)`, user, machine); err != nil {
		t.Fatal(err)
	}
	repo := NewRuntimeRepository(store, time.Minute)
	version := func() string {
		t.Helper()
		var v sql.NullString
		if err := store.SQL().QueryRowContext(ctx, `SELECT reporter_version FROM paperboat.user_machines WHERE id=$1`, machine).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v.String
	}
	if err := repo.RecordRuntimeObservation(ctx, RuntimeObservation{EnvironmentID: machine, MachineID: machine, ReporterVersion: "2026.10.25.1"}); err != nil {
		t.Fatal(err)
	}
	if got := version(); got != "2026.10.25.1" {
		t.Fatalf("reporter version = %q, want 2026.10.25.1", got)
	}
	if err := repo.RecordRuntimeObservation(ctx, RuntimeObservation{EnvironmentID: machine, MachineID: machine}); err != nil {
		t.Fatal(err)
	}
	if got := version(); got != "2026.10.25.1" {
		t.Fatalf("an observation without a version cleared it: %q", got)
	}
}
