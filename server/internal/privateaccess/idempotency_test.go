package privateaccess

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// testIdempotency is one shared database in unit tests.
type testIdempotency struct {
	mu    sync.Mutex
	bound map[string]string
}

func newTestIdempotency() *testIdempotency { return &testIdempotency{bound: map[string]string{}} }

func (s *testIdempotency) Bind(_ context.Context, account, session, key, fingerprint string, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := account + "\x00" + session + "\x00" + key
	if existing, ok := s.bound[k]; ok {
		return existing == fingerprint, nil
	}
	s.bound[k] = fingerprint
	return true, nil
}

// F21-54: an idempotency key bound on one server instance refuses a different body on another, keeps
// accepting the same body, and frees itself when it expires. Before, each instance kept its own cache.
func TestPrivateAccessIdempotencyKeyBindsAcrossInstancesOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run private-access idempotency on PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	first, _ := NewSQLIdempotencyStore(database)
	second, _ := NewSQLIdempotencyStore(database)
	account := fmt.Sprintf("acct_idem_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = database.Pool().Exec(ctx, `DELETE FROM paperboat.private_access_idempotency WHERE account_id=$1`, account)
	})
	live := time.Now().Add(time.Minute)
	if ok, err := first.Bind(ctx, account, "session_1", "key_1", "body_a", live); err != nil || !ok {
		t.Fatalf("first bind = %v %v", ok, err)
	}
	if ok, err := second.Bind(ctx, account, "session_1", "key_1", "body_b", live); err != nil || ok {
		t.Fatalf("another instance accepted a different body for the same key: %v %v", ok, err)
	}
	if ok, err := second.Bind(ctx, account, "session_1", "key_1", "body_a", live); err != nil || !ok {
		t.Fatalf("another instance refused the same body: %v %v", ok, err)
	}
	if _, err := database.Pool().Exec(ctx, `UPDATE paperboat.private_access_idempotency SET expires_at=now()-interval '1 second' WHERE account_id=$1`, account); err != nil {
		t.Fatal(err)
	}
	if ok, err := second.Bind(ctx, account, "session_1", "key_1", "body_b", live); err != nil || !ok {
		t.Fatalf("an expired key was not freed: %v %v", ok, err)
	}
	if err := PurgeIdempotency(ctx, database); err != nil {
		t.Fatal(err)
	}
}
