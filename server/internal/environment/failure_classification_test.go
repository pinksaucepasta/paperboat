package environment

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestNoRowsClassificationPreservesOperationalSiblings(t *testing.T) {
	if got := normalizeNoRows(pgx.ErrNoRows); got != sql.ErrNoRows {
		t.Fatalf("pure pgx no-row error = %v, want database/sql sentinel", got)
	}
	queryFailure := errors.Join(pgx.ErrNoRows, errors.New("query transport failed"))
	if got := normalizeNoRows(queryFailure); got != queryFailure {
		t.Fatalf("mixed query error was replaced: %v", got)
	}
	if !cleanTransactionNoRows(sql.ErrNoRows) {
		t.Fatal("bare no-row result after clean rollback was not recognized")
	}
	cleanupFailure := errors.Join(sql.ErrNoRows, errors.New("rollback failed"))
	if cleanTransactionNoRows(cleanupFailure) {
		t.Fatal("failed transaction cleanup was converted to not-found")
	}
}

func TestAuthorityCursorClassificationPreservesDatabaseFailures(t *testing.T) {
	if err := checkAuthorityCursor("sha256:expected", "sha256:expected", nil); err != nil {
		t.Fatalf("valid cursor: %v", err)
	}
	if err := checkAuthorityCursor("sha256:other", "sha256:expected", nil); !errors.Is(err, ErrAuthorityFork) {
		t.Fatalf("wrong cursor error = %v, want authority fork", err)
	}
	if err := checkAuthorityCursor("", "sha256:expected", pgx.ErrNoRows); !errors.Is(err, ErrAuthorityFork) {
		t.Fatalf("missing cursor error = %v, want authority fork", err)
	}
	queryFailure := errors.New("authority query unavailable")
	if err := checkAuthorityCursor("", "sha256:expected", queryFailure); err != queryFailure {
		t.Fatalf("query failure was replaced: %v", err)
	}
	mixedFailure := errors.Join(pgx.ErrNoRows, queryFailure)
	if err := checkAuthorityCursor("", "sha256:expected", mixedFailure); err != mixedFailure || errors.Is(err, ErrAuthorityFork) {
		t.Fatalf("mixed query failure was classified as a fork: %v", err)
	}
}

func TestTransitionScopeVersionKeepsQueryFailuresOperational(t *testing.T) {
	cases := []struct {
		name     string
		current  scopeRow
		err      error
		expected int64
		want     error
	}{
		{name: "new scope missing", err: sql.ErrNoRows, expected: 0},
		{name: "new scope exists", current: scopeRow{Version: 1}, expected: 0, want: ErrPrecondition},
		{name: "existing scope missing", err: sql.ErrNoRows, expected: 3, want: ErrPrecondition},
		{name: "version changed", current: scopeRow{Version: 2}, expected: 3, want: ErrPrecondition},
		{name: "version matches", current: scopeRow{Version: 3}, expected: 3},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := validateTransitionScopeVersion(test.current, test.err, test.expected); err != test.want {
				t.Fatalf("classification = %v, want %v", err, test.want)
			}
		})
	}
	queryFailure := errors.New("scope query unavailable")
	if err := validateTransitionScopeVersion(scopeRow{}, queryFailure, 0); err != queryFailure {
		t.Fatalf("new-scope query failure was replaced: %v", err)
	}
	mixedFailure := errors.Join(sql.ErrNoRows, queryFailure)
	if err := validateTransitionScopeVersion(scopeRow{}, mixedFailure, 0); err != mixedFailure {
		t.Fatalf("mixed scope query failure was converted to precondition: %v", err)
	}
}

func TestTransitionReplayLookupPreservesDatabaseFailures(t *testing.T) {
	missing := ErrOperationConflict
	if got := transitionLookupFailure(pgx.ErrNoRows, missing); got != missing {
		t.Fatalf("missing replay error = %v, want %v", got, missing)
	}
	if got := transitionLookupFailure(sql.ErrNoRows, missing); got != missing {
		t.Fatalf("normalized missing replay error = %v, want %v", got, missing)
	}
	queryFailure := errors.New("replay query unavailable")
	if got := transitionLookupFailure(queryFailure, missing); got != queryFailure {
		t.Fatalf("replay query failure was replaced: %v", got)
	}
	mixedFailure := errors.Join(pgx.ErrNoRows, queryFailure)
	if got := transitionLookupFailure(mixedFailure, missing); got != mixedFailure || errors.Is(got, missing) {
		t.Fatalf("mixed replay query failure was classified as absence: %v", got)
	}
}
