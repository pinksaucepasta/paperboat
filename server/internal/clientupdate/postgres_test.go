package clientupdate

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Run only against an isolated remote test database. It restores the singleton
// and removes mutable receipts. Immutable audits and their actor remain for
// isolated database teardown; no deployment DB is permitted.
func TestClientUpdatePolicyPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	if e = db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	s := New(store, audit.NewWriter(store), "")
	s.ceiling = func() (string, error) { return "2026.10.8.10", nil }
	original, e := s.Get(ctx)
	if e != nil {
		t.Fatal(e)
	}
	actor := "operator_" + uuid.NewString()
	if _, e = store.Pool().Exec(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash) VALUES($1,$2,$3)`, actor, actor+"@example.test", "test-fixture-unused-password"); e != nil {
		t.Fatal(e)
	}
	defer func() {
		_, err := store.Pool().Exec(ctx, `UPDATE paperboat.client_update_policy SET revision=$1, minimum_version=$2, reason=$3, enforce_at=$4 WHERE singleton`, original.Revision, original.MinimumVersion, original.Reason, original.EnforceAt)
		if err != nil {
			t.Error(err)
		}
		_, err = store.Pool().Exec(ctx, `DELETE FROM paperboat.client_update_policy_requests WHERE actor_id=$1`, actor)
		if err != nil {
			t.Error(err)
		}
		// Immutable audit events and their operator actor remain until the
		// owning isolated test database is dropped by the test lead.

	}()
	input := Input{ExpectedRevision: original.Revision, MinimumVersion: "2026.10.8.2", Reason: "Support test"}
	created, e := s.Put(ctx, actor, "set-test-key", "test_reference", input)
	if e != nil {
		t.Fatal(e)
	}
	if created.Revision != original.Revision+1 {
		t.Fatal(created)
	}
	// Replay must survive a release-origin outage and preserve original revision.
	s.ceiling = func() (string, error) { return "", errors.New("origin down") }
	replay, e := s.Put(ctx, actor, "set-test-key", "test_reference", input)
	if e != nil || replay.Revision != created.Revision {
		t.Fatal(replay, e)
	}
	changed := input
	changed.Reason = "different intent"
	if _, e = s.Put(ctx, actor, "set-test-key", "test_reference", changed); !errors.Is(e, ErrIdempotency) {
		t.Fatalf("idempotency %v", e)
	}
	if _, e = s.Put(ctx, actor, "stale-test-key", "test_reference", input); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale %v", e)
	}
	input.ExpectedRevision = created.Revision
	originFailure := errors.New("origin down")
	s.ceiling = func() (string, error) { return "", originFailure }
	if _, e = s.Put(ctx, actor, "unavailable-test-key", "test_reference", input); !errors.Is(e, ErrReleaseUnavailable) || !errors.Is(e, originFailure) || e.Error() != ErrReleaseUnavailable.Error() {
		t.Fatalf("unavailable failure lost safe cause: %v", e)
	}
	s.ceiling = func() (string, error) { return "2026.10.8.10", nil }
	input.MinimumVersion = "2026.10.8.11"
	if _, e = s.Put(ctx, actor, "future-test-key", "test_reference", input); !errors.Is(e, ErrFloorUnavailable) {
		t.Fatalf("future %v", e)
	}
	input.MinimumVersion = "2026.10.8.3"
	broken := New(store, nil, "")
	broken.ceiling = s.ceiling
	if _, e = broken.Put(ctx, actor, "audit-fail-test-key", "test_reference", input); e == nil {
		t.Fatal("audit failure accepted")
	}
	after, e := s.Get(ctx)
	if e != nil || after.Revision != created.Revision || after.MinimumVersion != created.MinimumVersion {
		t.Fatalf("atomicity %+v %v", after, e)
	}
	// Two writers with the same expected revision cannot both commit.
	results := make(chan error, 2)
	for _, key := range []string{"race-policy-key-a", "race-policy-key-b"} {
		go func(key string) { _, err := s.Put(ctx, actor, key, "test_reference", input); results <- err }(key)
	}
	winners, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			winners++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("concurrent results winners=%d conflicts=%d", winners, conflicts)
	}
	s.ceiling = func() (string, error) { return "", errors.New("origin down") }
	cleared, e := s.Put(ctx, actor, "clear-test-key", "test_reference", Input{ExpectedRevision: created.Revision + 1})
	if e != nil || cleared.MinimumVersion != "" {
		t.Fatal(cleared, e)
	}
	var count int
	if e = store.Pool().QueryRow(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE actor_user_id=$1 AND event_type='client_update_policy.changed'`, actor).Scan(&count); e != nil || count != 3 {
		t.Fatalf("audit count %d %v", count, e)
	}
}
