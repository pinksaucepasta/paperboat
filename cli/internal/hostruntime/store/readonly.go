package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store/storesqlc"
)

// OpenReadOnly reads an existing store without creating, migrating, changing
// permissions, or checkpointing it. The caller owns exclusion from session writers.
func OpenReadOnly(ctx context.Context, root string) (_ *Store, resultErr error) {
	defer protectStoreError(&resultErr)
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrCorrupt
	}
	path := filepath.Join(root, "state.db")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrCorrupt
	}
	dsn := &url.URL{Scheme: "file", Path: sqliteURIPath(path)}
	query := dsn.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(ON)")
	query.Add("_pragma", "busy_timeout(5000)")
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, db.Close())
		}
	}()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version != CurrentVersion {
		return nil, ErrIncompatible
	}
	if err := validateDatabaseSchema(ctx, db); err != nil {
		return nil, err
	}
	s := &Store{db: db, q: storesqlc.New(db), readOnly: true}
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// BoundedSessions observes the count and records in the same read transaction.
func (s *Store) BoundedSessionMetadata(ctx context.Context, maximum, maximumBytes int) (_ []Session, resultErr error) {
	defer protectStoreError(&resultErr)
	if maximum < 1 || maximumBytes < 1 {
		return nil, ErrResultTooLarge
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	var count, size int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(length(CAST(id AS BLOB)) + length(CAST(name AS BLOB)) + length(CAST(cwd AS BLOB)) + COALESCE(length(CAST(exit_signal AS BLOB)), 0)), 0) FROM sessions").Scan(&count, &size); err != nil {
		return nil, err
	}
	if count > maximum || size > maximumBytes {
		return nil, ErrResultTooLarge
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,name,cwd,columns,rows,state,generation,exit_code,COALESCE(exit_signal,''),exited_at,earliest_sequence,latest_sequence FROM sessions ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	records := make([]Session, 0, count)
	for rows.Next() {
		var record Session
		var columns, lines, generation, earliest, latest int64
		var exitCode, exitedAt sql.NullInt64
		if err := rows.Scan(&record.ID, &record.Name, &record.CWD, &columns, &lines, &record.State, &generation, &exitCode, &record.ExitSignal, &exitedAt, &earliest, &latest); err != nil {
			return nil, err
		}
		if columns < 1 || columns > 65535 || lines < 1 || lines > 65535 || generation < 1 || earliest < 0 || latest < earliest {
			return nil, ErrCorrupt
		}
		record.Columns, record.Rows = uint16(columns), uint16(lines)
		record.Generation, record.EarliestSequence, record.LatestSequence = uint64(generation), uint64(earliest), uint64(latest)
		if exitCode.Valid {
			value := int(exitCode.Int64)
			record.ExitCode = &value
		}
		if exitedAt.Valid {
			value := time.Unix(0, exitedAt.Int64).UTC()
			record.ExitedAt = &value
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return records, tx.Commit()
}
