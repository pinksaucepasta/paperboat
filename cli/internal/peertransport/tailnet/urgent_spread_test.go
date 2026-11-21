package tailnet

import (
	"testing"
	"time"
)

// Announced renewals of an account's endpoints spread over urgentRefreshSpread instead of arriving as one burst.
func TestUrgentSpreadBounded(t *testing.T) {
	seen := map[bool]bool{}
	for range 1000 {
		d := urgentSpreadRandom()
		if d < 0 || d >= urgentRefreshSpread {
			t.Fatalf("spread %v outside [0,%v)", d, urgentRefreshSpread)
		}
		seen[d < urgentRefreshSpread/2] = true
	}
	if len(seen) != 2 {
		t.Fatal("spread does not cover its range")
	}
}

// Other tests time announced renewals exactly; the spread is checked above.
func init() { urgentSpread = func() time.Duration { return 0 } }
