package handover

import (
	"net"
	"testing"
	"time"
)

// No serving process: a fresh start inherits nothing.
func TestInheritWithoutAServingProcessIsAFreshStart(t *testing.T) {
	inheritance, err := Inherit(t.TempDir())
	if err != nil || inheritance != nil {
		t.Fatalf("inherit = %v, %v; want a fresh start", inheritance, err)
	}
}

var _ net.Listener = (*listener)(nil)

func TestRemainingAfterTakeover(t *testing.T) {
	defer tookOverAt.Store(tookOverAt.Load())
	tookOverAt.Store(0)
	if got := RemainingAfterTakeover(time.Minute); got != 0 {
		t.Fatalf("without a takeover = %v, want 0", got)
	}
	tookOverAt.Store(time.Now().Add(-4 * time.Second).UnixNano())
	if got := RemainingAfterTakeover(10 * time.Second); got < 5*time.Second || got > 6*time.Second {
		t.Fatalf("4 s into a 10 s window = %v, want about 6 s", got)
	}
	if got := RemainingAfterTakeover(3 * time.Second); got != 0 {
		t.Fatalf("after the window = %v, want 0", got)
	}
}
