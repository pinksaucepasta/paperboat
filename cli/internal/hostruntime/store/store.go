package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store/storesqlc"
)

const (
	CurrentVersion          = 1
	MaxOperationResultBytes = 64 << 10
)

var (
	ErrIncompatible   = errors.New("store version is incompatible")
	ErrCorrupt        = errors.New("store is corrupt")
	ErrConflict       = errors.New("store state conflict")
	ErrNotFound       = errors.New("store record not found")
	ErrReplayGap      = errors.New("replay gap")
	ErrResultTooLarge = errors.New("operation result is too large")
)

type Config struct {
	Root        string
	FailureHook func(string) error
}
type Store struct {
	readOnly bool
	db       *sql.DB
	q        *storesqlc.Queries
	hook     func(string) error
}

type Session struct {
	ID               string
	Name             string
	CWD              string
	CommandPath      string
	CommandArgs      []string
	CommandEnv       []string
	Columns          uint16
	Rows             uint16
	State            string
	Generation       uint64
	ExitCode         *int
	ExitSignal       string
	ExitedAt         *time.Time
	EarliestSequence uint64
	LatestSequence   uint64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}
type OutputEvent struct {
	Channel       byte
	StartSequence uint64
	EndSequence   uint64
	Data          []byte
}
type GapError struct{ Requested, Earliest, Latest uint64 }

func (e *GapError) Error() string {
	return fmt.Sprintf("%v: requested %d retained [%d,%d)", ErrReplayGap, e.Requested, e.Earliest, e.Latest)
}
func (e *GapError) Unwrap() error { return ErrReplayGap }

type InputDecision struct {
	SessionID    string
	ClientID     string
	AttachmentID string
	Generation   uint64
	InputID      string
	Hash         []byte
	Status       string
	BytesWritten int
	ErrorCode    string
	CreatedAt    time.Time
}
type OperationResult struct {
	OperationID string
	RequestHash []byte
	State       string
	Result      []byte
	ErrorCode   string
	CompletedAt time.Time
	ExpiresAt   time.Time
}

func Open(ctx context.Context, config Config) (openedStore *Store, resultErr error) {
	defer protectStoreError(&resultErr)
	if !filepath.IsAbs(config.Root) {
		return nil, ErrCorrupt
	}
	if err := ensureDirectory(config.Root); err != nil {
		return nil, err
	}
	path := filepath.Join(config.Root, "state.db")
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return nil, ErrCorrupt
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dsnURL := &url.URL{Scheme: "file", Path: sqliteURIPath(path)}
	query := dsnURL.Query()
	// These PRAGMAs are connection-local. Put them in the DSN so every
	// database/sql connection gets the same locking and integrity policy.
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "synchronous(FULL)")
	// Every transaction takes the write lock at BEGIN, so busy_timeout applies. A deferred
	// transaction that reads first fails at once with SQLITE_BUSY when it upgrades to a write while
	// the other connection commits, as during a terminal's output burst.
	query.Set("_txlock", "immediate")
	dsnURL.RawQuery = query.Encode()
	dsn := dsnURL.String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Schema validation can issue nested reads while Goose still owns its
	// connection. A single-connection pool deadlocks on Linux/arm64.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	store := &Store{db: db, q: storesqlc.New(db), hook: config.FailureHook}
	defer func() {
		if resultErr != nil {
			resultErr = classifyDBError(storeFailure("runtime state open failed", resultErr, db.Close()))
		}
	}()
	if err := store.configure(ctx); err != nil {
		return nil, err
	}
	if err := store.migrate(ctx); err != nil {
		return nil, err
	}
	if err := validateDatabaseSchema(ctx, db); err != nil {
		return nil, err
	}
	if err := secureStoreFile(path); err != nil {
		return nil, err
	}
	if err := store.check(ctx); err != nil {
		return nil, err
	}
	// After migration one connection carries every statement, so writers queue in Go instead of
	// racing for SQLite's unfair write lock: a session's close waits its turn behind another
	// session's output burst instead of failing with SQLITE_BUSY after busy_timeout.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return store, nil
}

// sqliteURIPath keeps a Windows volume path in the URI path component. Without
// the leading slash, net/url serializes C: as a URI authority and modernc
// SQLite rejects the resulting file://C:... DSN before it can open state.db.
func sqliteURIPath(path string) string {
	if volume := filepath.VolumeName(path); volume != "" {
		return "/" + strings.ReplaceAll(path, `\`, "/")
	}
	return path
}

func (s *Store) Close() (resultErr error) {
	defer protectStoreError(&resultErr)
	if s.readOnly {
		return s.db.Close()
	}
	_, checkpointErr := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return errors.Join(checkpointErr, s.db.Close())
}

func (s *Store) configure(ctx context.Context) (resultErr error) {
	defer protectStoreError(&resultErr)
	for _, statement := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000"} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) migrate(ctx context.Context) (resultErr error) {
	defer protectStoreError(&resultErr)
	return migrateDatabase(ctx, s.db, s.hook)
}
func (s *Store) check(ctx context.Context) (resultErr error) {
	defer protectStoreError(&resultErr)
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return ErrCorrupt
	}
	return nil
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrCorrupt
	}
	return secureStoreDirectory(path)
}
