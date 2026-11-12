package tunnelv1

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

type failingRow struct{ err error }

func (r failingRow) Scan(...any) error { return r.err }

type failingDB struct{ err error }

func (failingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (failingDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (d failingDB) QueryRow(context.Context, string, ...any) pgx.Row      { return failingRow(d) }

// A database failure while explaining a denied host is a failure, not "host not found": the caller answers 500, not 403.
func TestDeniedHostKeepsDatabaseFailuresApartFromNoRows(t *testing.T) {
	queries := dbsqlc.New(failingDB{err: errors.New("connection reset")})
	if err := deniedHost(context.Background(), queries, "acct", "host"); err == nil || errors.Is(err, ErrHostNotFound) {
		t.Fatalf("database failure = %v, want an error that is not ErrHostNotFound", err)
	}
	queries = dbsqlc.New(failingDB{err: pgx.ErrNoRows})
	if err := deniedHost(context.Background(), queries, "acct", "host"); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("no rows = %v, want ErrHostNotFound", err)
	}
}
