package teams

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMapTeamReadNotFoundPreservesOperationalCauses(t *testing.T) {
	if got := mapTeamReadNotFound(pgx.ErrNoRows); got != ErrNotFound {
		t.Fatalf("direct no-row sentinel mapped to %v, want ErrNotFound", got)
	}

	wrapped := fmt.Errorf("read team row: %w", pgx.ErrNoRows)
	if got := mapTeamReadNotFound(wrapped); got != wrapped {
		t.Fatalf("wrapped no-row cause changed: got %v", got)
	}

	joined := errors.Join(pgx.ErrNoRows, syscall.EIO)
	got := mapTeamReadNotFound(joined)
	if got != joined || !errors.Is(got, pgx.ErrNoRows) || !errors.Is(got, syscall.EIO) {
		t.Fatalf("joined row and I/O causes were not preserved: got %v", got)
	}
	if errors.Is(got, ErrNotFound) {
		t.Fatal("joined I/O failure was misclassified as an expected missing team")
	}
}
