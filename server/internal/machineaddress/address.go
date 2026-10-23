// Package machineaddress turns a machine's address lease slot (control_sync_leases.address_slot) into its local
// address in the one global pool 127.64.0.0/10 (PLAN 3b "Machine local addresses"). Slots below 65,536 are
// exactly the 127.100.x.y addresses leases had before the pool grew, so every existing lease keeps its address;
// higher slots continue with 127.101.0.0 and wrap to 127.64.0.0 after 127.127.255.255. Which slots may be leased
// is paperboat.machine_address_slot_allowed (migration 172).
package machineaddress

import "net/netip"

const (
	// PoolSize is the number of slots in 127.64.0.0/10.
	PoolSize = 1 << 22
	// legacyOffset is where 127.100.0.0 sits in the pool.
	legacyOffset = (100 - 64) << 16
)

// FromSlot is the address of slot, which must be in [0, PoolSize).
func FromSlot(slot int) netip.Addr {
	offset := (slot + legacyOffset) % PoolSize
	return netip.AddrFrom4([4]byte{127, byte(64 + offset>>16), byte(offset >> 8), byte(offset)})
}
