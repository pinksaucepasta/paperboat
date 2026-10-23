package machineaddress

import (
	"fmt"
	"net/netip"
	"testing"
)

// Every slot a lease could hold before the pool grew keeps its 127.100.x.y address.
func TestSlotsBelowTheOldPoolKeepTheirAddresses(t *testing.T) {
	for slot := 0; slot < 1<<16; slot++ {
		if got, want := FromSlot(slot).String(), fmt.Sprintf("127.100.%d.%d", slot/256, slot%256); got != want {
			t.Fatalf("slot %d: %s, want %s", slot, got, want)
		}
	}
}

// The pool covers 127.64.0.0/10 exactly once: 127.101.0.0 follows 127.100.255.255 and 127.64.0.0 follows
// 127.127.255.255.
func TestThePoolIsExactly127_64_0_0_10(t *testing.T) {
	pool := netip.MustParsePrefix("127.64.0.0/10")
	seen := make(map[netip.Addr]bool, PoolSize)
	for slot := 0; slot < PoolSize; slot++ {
		addr := FromSlot(slot)
		if !pool.Contains(addr) || seen[addr] {
			t.Fatalf("slot %d: %s outside the pool or used twice", slot, addr)
		}
		seen[addr] = true
	}
	for slot, want := range map[int]string{1 << 16: "127.101.0.0", (128-100)<<16 - 1: "127.127.255.255", (128 - 100) << 16: "127.64.0.0", PoolSize - 1: "127.99.255.255"} {
		if got := FromSlot(slot).String(); got != want {
			t.Errorf("slot %d: %s, want %s", slot, got, want)
		}
	}
}
