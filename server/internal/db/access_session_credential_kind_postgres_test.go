package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// The external-issuer credential path is gone (92e38f29a): migrations 467 and 468 leave one validated kind.
func TestAccessSessionCredentialKindAllowsOnlyControlPlaneSignedOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	var definition string
	var validated bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT pg_get_constraintdef(oid), convalidated FROM pg_constraint
 WHERE conrelid='paperboat.user_machine_access_sessions'::regclass AND conname='user_machine_access_sessions_credential_kind_check'`).Scan(&definition, &validated); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(definition, "external_issuer") || !strings.Contains(definition, "control_plane_signed") || !validated {
		t.Fatalf("credential_kind check = %q validated=%v", definition, validated)
	}
}
