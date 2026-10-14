package inspectaccess

import (
	"database/sql"
	"errors"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

func TestNoRowsClassificationDoesNotHideOperationalCause(t *testing.T) {
	for _, sentinel := range []error{pgx.ErrNoRows, sql.ErrNoRows} {
		if !isExactNoRows(sentinel) {
			t.Fatalf("exact no-row sentinel %T was not recognized", sentinel)
		}
		mixed := errors.Join(sentinel, syscall.EIO)
		if isExactNoRows(mixed) {
			t.Fatalf("mixed %T and EIO was classified as an expected absence", sentinel)
		}
		mapped := mapMissing(mixed)
		if mapped == ErrNotAuthorized || !errors.Is(mapped, sentinel) || !errors.Is(mapped, syscall.EIO) {
			t.Fatalf("mixed absence/storage error lost a cause: %v", mapped)
		}
	}
}

func TestAuthorizationNormalizationPreservesJoinedOperationalCause(t *testing.T) {
	for _, sentinel := range []error{ErrNotAuthorized, teams.ErrForbidden, teams.ErrNotFound} {
		if got := normalizeAuthorizationError(sentinel); got != ErrNotAuthorized {
			t.Fatalf("pure %T = %v, want ErrNotAuthorized", sentinel, got)
		}
		mixed := errors.Join(sentinel, syscall.EIO)
		got := normalizeAuthorizationError(mixed)
		if got == ErrNotAuthorized || !errors.Is(got, sentinel) || !errors.Is(got, syscall.EIO) {
			t.Fatalf("mixed %T and EIO was normalized to a denial or lost a cause: %v", sentinel, got)
		}
	}
}

func TestExactExpectedRejectionRejectsMixedAndUncomparableErrors(t *testing.T) {
	if !IsExactExpectedRejection(ErrInvalid) || !IsExactExpectedRejection(ErrNotAuthorized) {
		t.Fatal("direct inspector rejection was not recognized")
	}
	if IsExactExpectedRejection(errors.Join(ErrNotAuthorized, syscall.EIO)) {
		t.Fatal("joined operational error was classified as an expected rejection")
	}
	if IsExactExpectedRejection(uncomparableInspectorError{parts: []string{"failure"}}) {
		t.Fatal("uncomparable error was treated as a direct expected rejection")
	}
}

type uncomparableInspectorError struct{ parts []string }

func (e uncomparableInspectorError) Error() string { return "inspector failure" }
