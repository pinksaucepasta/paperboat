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

func validateDatabaseSchema(ctx context.Context, database *sql.DB) error {
	options := &squibble.DigestOptions{IgnoreTables: []string{"goose_db_version"}}
	if err := squibble.Validate(ctx, database, canonicalSchema, options); err != nil {
		if _, mismatch := err.(squibble.ValidationError); mismatch {
			return storeFailure("runtime state schema is invalid", ErrCorrupt, err)
		}
		return storeFailure("runtime state schema check failed", err)
	}
	// squibble compares columns, not CHECK bodies: a store created with the retired 50 MiB per-file limit would
	// open and then refuse every larger file. Zero compat: such a machine is reinstalled, so refuse it here.
	var table string
	if err := database.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='file_transfers'`).Scan(&table); err != nil {
		return storeFailure("runtime state schema check failed", err)
	}
	if strings.Contains(table, "52428800") {
		return storeFailure("runtime state has the retired 50 MiB file transfer limit; reinstall this machine (pb uninstall, then pb install and pair)", ErrCorrupt)
	}
	return nil
}
