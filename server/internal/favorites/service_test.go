package favorites

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestSetRejectsInvalidInputBeforeDatabaseAccess(t *testing.T) {
	service := NewService(nil)
	if err := service.Set(context.Background(), "user_1", "other", "resource_1", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown favorite kind error = %v", err)
	}
	if err := service.Set(context.Background(), "user_1", "machine", "  ", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty favorite ID error = %v", err)
	}
}

func TestFavoriteRowAbsentRequiresExactPGXNoRows(t *testing.T) {
	if !favoriteRowAbsent(pgx.ErrNoRows) {
		t.Fatal("pgx no-rows sentinel was not recognized")
	}
	if favoriteRowAbsent(errors.Join(pgx.ErrNoRows, syscall.EIO)) {
		t.Fatal("joined operational failure was treated as a missing resource")
	}
	if favoriteRowAbsent(fmt.Errorf("query: %w", pgx.ErrNoRows)) {
		t.Fatal("unexpectedly wrapped no-rows failure was treated as a missing resource")
	}
}
