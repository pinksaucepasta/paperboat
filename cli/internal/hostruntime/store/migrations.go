package store

import (
	"context"
	"database/sql"
	"embed"
	"strings"

	"sync"

	"github.com/pressly/goose/v3"
	"github.com/tailscale/squibble"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed schema.sql
var canonicalSchema string

var migrationMu sync.Mutex

func migrateDatabase(ctx context.Context, database *sql.DB, hook func(string) error) error {
	var version int
	if err := database.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > CurrentVersion {
		return ErrIncompatible
	}
	if version == 0 && hook != nil {
		if err := hook("migration_before_commit"); err != nil {
			return err
		}
	}

	// Goose's package configuration is process-global, so serialize configuration
	// and execution across concurrently opened runtime stores.
	migrationMu.Lock()
	defer migrationMu.Unlock()
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("goose_db_version")
	if err := goose.SetDialect("sqlite3"); err != nil {
		return storeFailure("runtime store migration dialect failed", err)
	}
	if err := goose.UpContext(ctx, database, "migrations"); err != nil {
		return storeFailure("runtime store migration failed", err)
	}
	return nil
}

// unboundedFileTransferSchema is the canonical schema with the per-file 50 MiB CHECK lifted, the table a later
// release migrates to for 2 GB and unlimited transfers (PLAN 3b File transfer modes). Accepting it now keeps a
// rollback from that release working: this runtime starts on the migrated store instead of calling it corrupt.
var unboundedFileTransferSchema = strings.Replace(canonicalSchema, "CHECK(size BETWEEN 0 AND 52428800)", "CHECK(size >= 0)", 1)

func validateDatabaseSchema(ctx context.Context, database *sql.DB) error {
	options := &squibble.DigestOptions{IgnoreTables: []string{"goose_db_version"}}
	if squibble.Validate(ctx, database, unboundedFileTransferSchema, options) == nil {
		return nil
	}
	if err := squibble.Validate(ctx, database, canonicalSchema, options); err != nil {
		if _, mismatch := err.(squibble.ValidationError); mismatch {
			return storeFailure("runtime state schema is invalid", ErrCorrupt, err)
		}
		return storeFailure("runtime state schema check failed", err)
	}
	return nil
}
