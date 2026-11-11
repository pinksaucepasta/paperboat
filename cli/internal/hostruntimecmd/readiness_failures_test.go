package hostruntimecmd

import (
	"errors"
	"testing"
)

func TestReadinessFailuresNameFirstAndLast(t *testing.T) {
	first, last := errors.New("capability rejected"), errors.New("connection refused")
	if got := readinessFailures(first, first, 1); got != first {
		t.Fatalf("one failed probe reported as %v", got)
	}
	if got := readinessFailures(nil, nil, 0); got != nil {
		t.Fatalf("no failure reported as %v", got)
	}
	both := readinessFailures(first, last, 5)
	if !errors.Is(both, first) || !errors.Is(both, last) {
		t.Fatalf("both failures expected, got %v", both)
	}
}
