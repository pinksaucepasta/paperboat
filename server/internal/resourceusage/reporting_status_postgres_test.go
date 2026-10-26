package resourceusage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Every native usage report (~4/min per machine) repeats the machine's reporting status. Before, each one rewrote
// native_usage_reporting_status (~430 row versions an hour per machine on staging, PLAN 3b). Now an unchanged
// status is written only to refresh last_seen once it is half stale; a new loss is written at once.
func TestUnchangedNativeReportingStatusWritesNoRowOnPostgres(t *testing.T) {
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
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine := "rs_owner_"+suffix, "rs_machine_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($2,$1,$2,'linux','amd64','/workspace','online','occupied',true,1)`, owner, machine); err != nil {
		t.Fatal(err)
	}
	record := func(batch NativeBatch, now time.Time) {
		t.Helper()
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			return recordNativeReportingStatus(ctx, tx, machine, 1, batch, 0, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	xmin := func() string {
		t.Helper()
		var x string
		if err := store.SQL().QueryRowContext(ctx, `SELECT xmin::text FROM paperboat.native_usage_reporting_status WHERE machine_id=$1`, machine).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	now := time.Now().UTC()
	record(NativeBatch{}, now)
	stored := xmin()
	for i := range 3 {
		record(NativeBatch{}, now.Add(time.Duration(i+1)*10*time.Second))
	}
	if after := xmin(); after != stored {
		t.Fatalf("3 unchanged reports rewrote the status (xmin %s -> %s)", stored, after)
	}
	record(NativeBatch{UnrecordedBytes: 7}, now.Add(40*time.Second))
	lost := xmin()
	if lost == stored {
		t.Fatal("a new loss was not written")
	}
	record(NativeBatch{UnrecordedBytes: 7}, now.Add(ReportingFreshness))
	if after := xmin(); after == lost {
		t.Fatal("a half-stale status was not refreshed")
	}
}
