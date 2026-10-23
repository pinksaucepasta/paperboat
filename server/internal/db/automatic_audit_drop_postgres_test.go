package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Migration 167 deletes the automatic audit events no server writes any more (config syncs, a host's preview
// renewals), most of them younger than the 30 days past which the prune job may delete: the audit guard refused
// it on a production dump. The guard's exception for them is narrow: under paperboat.audit_drop_automatic only
// those event types go, at any age; a person's preview renewal and every other event stay append-only.
func TestAutomaticAuditEventsAloneMayBeDroppedOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	events := map[string][2]string{ // id -> event type, actor type
		"auto_cred_" + s:   {"config.credential_issued", "system"},
		"auto_repo_" + s:   {"config.repository_access_issued", "system"},
		"auto_status_" + s: {"config.status_recorded", "system"},
		"auto_renew_" + s:  {"preview.lease_renewed", "host"},
		"user_renew_" + s:  {"preview.lease_renewed", "user"},
		"other_" + s:       {"preview.lease_stopped", "system"},
	}
	for id, e := range events {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.audit_events(id,actor_type,event_type,resource_type,resource_id,metadata)
 VALUES($1,$3,$2,'audit_drop_probe',$1,'{}')`, id, e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	drop := func(id string) error {
		tx, err := store.SQL().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `SELECT set_config('paperboat.audit_drop_automatic','on',true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM paperboat.audit_events WHERE id=$1`, id); err != nil {
			return err
		}
		return tx.Commit()
	}
	for id, e := range events {
		err := drop(id)
		automatic := id[:5] == "auto_"
		if automatic && err != nil {
			t.Errorf("%s (%s by %s) could not be dropped: %v", id, e[0], e[1], err)
		}
		if !automatic && !isCode(err, "55000") {
			t.Errorf("%s (%s by %s) dropped under the automatic exception: %v, want append-only", id, e[0], e[1], err)
		}
	}
	// Without the setting nothing within retention goes.
	if _, err := store.SQL().ExecContext(ctx, `DELETE FROM paperboat.audit_events WHERE id=$1`, "user_renew_"+s); !isCode(err, "55000") {
		t.Fatalf("a recent event was deleted without any exception: %v", err)
	}
}
