package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"hash/fnv"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const migrationsDir = "migrations"

func Migrate(ctx context.Context, d *DB) error {
	if err := d.Direct().Ping(ctx); err != nil {
		return fmt.Errorf("ping direct database: %w", err)
	}
	// Migrations run on their own pool, on the direct DSN: the migration lock below is a session lock, and the
	// request pool's statement and lock timeouts would cut index builds.
	migrationPool, err := pgxpool.NewWithConfig(ctx, d.migrationConfig)
	if err != nil {
		return fmt.Errorf("open migration pool: %w", err)
	}
	defer migrationPool.Close()
	migrations := stdlib.OpenDBFromPool(migrationPool)
	defer migrations.Close()
	conn, err := migrations.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey()); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey())
	if _, err := conn.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS paperboat`); err != nil {
		return fmt.Errorf("ensure paperboat schema: %w", err)
	}

	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set migration dialect: %w", err)
	}
	if err := checkBaselineHistory(ctx, migrations); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, migrations, migrationsDir); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

// This pre-release baseline is only for an empty database. Old migration
// histories must not be mistaken for the new version 1.
func checkBaselineHistory(ctx context.Context, database *sql.DB) error {
	var baselineApplied, existingObjects, historyApplied, baselineObjects bool
	err := database.QueryRowContext(ctx, `
SELECT EXISTS (
  SELECT 1 FROM paperboat.goose_db_version
  WHERE version_id = 1 AND is_applied
), EXISTS (
  SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='paperboat' AND c.relkind IN ('r','p','v','m','S')
    AND c.relname <> 'goose_db_version'
), (
  SELECT EXISTS (
    SELECT 1 FROM paperboat.goose_db_version
    WHERE version_id > 0 AND is_applied
  )
), (
  to_regclass('paperboat.tunnel_platform_certificate_targets') IS NOT NULL
  AND to_regclass('paperboat.peer_endpoint_enrollment_requests') IS NOT NULL
  AND to_regclass('paperboat.user_machine_access_sessions_revocation_expiry') IS NOT NULL
  AND to_regclass('paperboat.cli_refresh_tokens_session') IS NOT NULL
  AND to_regclass('paperboat.user_machine_terminal_session_operations_pending_created') IS NOT NULL
)`).Scan(&baselineApplied, &existingObjects, &historyApplied, &baselineObjects)
	if err != nil {
		// Goose has not initialized the version table on a fresh database.
		var historyExists bool
		if checkErr := database.QueryRowContext(ctx, `SELECT to_regclass('paperboat.goose_db_version') IS NOT NULL`).Scan(&historyExists); checkErr != nil {
			return fmt.Errorf("check migration history: %w", checkErr)
		}
		if historyExists {
			return fmt.Errorf("read migration history: %w", err)
		}
		if err := database.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='paperboat' AND c.relkind IN ('r','p','v','m','S')
)`).Scan(&existingObjects); err != nil {
			return fmt.Errorf("check existing schema objects: %w", err)
		}
	}
	if (existingObjects || historyApplied) && (!baselineApplied || !baselineObjects) {
		return fmt.Errorf("database predates baseline 1: restore it from backup or replace it with a fresh database before running migrations")
	}
	return nil
}

func migrationLockKey() int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("paperboat-server:migrations"))
	return int64(hash.Sum64())
}
