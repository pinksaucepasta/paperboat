package machineaddress

import (
	"fmt"
	"testing"
)

// A slot is an address in 127.100/16 only: every account numbers its own machines (migration 199).
func TestSlotsAreAddressesIn127_100(t *testing.T) {
	for slot := 0; slot < 1<<16; slot++ {
		if got, want := FromSlot(slot).String(), fmt.Sprintf("127.100.%d.%d", slot/256, slot%256); got != want {
			t.Fatalf("slot %d: %s, want %s", slot, got, want)
		}
	}
}
