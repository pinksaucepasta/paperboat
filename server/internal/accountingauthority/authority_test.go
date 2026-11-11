package accountingauthority

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Without the authoritative quota check, admission cannot be judged: it fails closed instead of guessing from a
// single period row (which ignored top-ups and budget segments).
func TestRecordFailsClosedWithoutAQuotaChecker(t *testing.T) {
	now := time.Now()
	b := Binding{ID: "a", Node: "n", Environment: "e", Route: "r", User: "u", Revision: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := Record(context.Background(), nil, b, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Record without a quota checker = %v, want ErrUnavailable", err)
	}
}
