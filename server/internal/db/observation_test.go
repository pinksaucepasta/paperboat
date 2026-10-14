package db

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestQueryObservationOmitsSQLAndPreservesOutcomes(t *testing.T) {
	observer := &queryObserver{}
	store := &DB{observer: observer}
	var outcomes []string
	type key struct{}
	store.SetQueryObserver(func(ctx context.Context) (context.Context, func(string)) {
		if ctx.Value(key{}) != "parent" {
			t.Fatal("lost parent context")
		}
		return ctx, func(outcome string) { outcomes = append(outcomes, outcome) }
	})
	ctx := context.WithValue(context.Background(), key{}, "parent")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	deadline, cancelDeadline := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancelDeadline()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{name: "success", ctx: ctx, want: "success"},
		{name: "failure", ctx: ctx, err: errors.New("database operation failed"), want: "failed"},
		{name: "cancel", ctx: canceled, err: context.Canceled, want: "canceled"},
		{name: "wrapped cancel", ctx: canceled, err: fmt.Errorf("query: %w", context.Canceled), want: "canceled"},
		{name: "deadline", ctx: deadline, err: context.DeadlineExceeded, want: "failed"},
		{name: "cancel with independent failure", ctx: canceled, err: errors.Join(context.Canceled, errors.New("database operation failed")), want: "failed"},
		{name: "completed before later cancel", ctx: canceled, want: "success"},
	}
	for _, tc := range cases {
		query := observer.TraceQueryStart(tc.ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT SECRET_SQL", Args: []any{"SECRET_ARGUMENT"}})
		observer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{Err: tc.err})
		if got := outcomes[len(outcomes)-1]; got != tc.want {
			t.Fatalf("%s outcome=%q, want %q", tc.name, got, tc.want)
		}
	}
	store.SetQueryObserver(nil)
	observer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{})
}

func TestQueryObservationPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var mu sync.Mutex
	var outcomes []string
	store.SetQueryObserver(func(ctx context.Context) (context.Context, func(string)) {
		return ctx, func(outcome string) { mu.Lock(); outcomes = append(outcomes, outcome); mu.Unlock() }
	})
	if _, err = store.Pool().Exec(t.Context(), "SELECT $1::text", "PRIVATE_QUERY_ARGUMENT"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(t.Context(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool().Exec(t.Context(), "SELECT 1/0"); err == nil {
		t.Fatal("fixture must fail")
	}
	mu.Lock()
	defer mu.Unlock()
	success, failed := 0, 0
	for _, outcome := range outcomes {
		if outcome == "success" {
			success++
		}
		if outcome == "failed" {
			failed++
		}
	}
	if success < 2 || failed != 1 {
		t.Fatalf("query observer missed pool/sql/failure: %v", outcomes)
	}
}
