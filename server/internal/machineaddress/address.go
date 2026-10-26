// Package machineaddress turns a machine's address lease slot (control_sync_leases.address_slot) into its local
// address. Leases are per account (PLAN 3b "Multi-account hosts"): every account numbers its own machines from 127.100.0.2,
// so a slot is an address in 127.100/16 only (last octet 1..254; paperboat.ensure_machine_address_leases, migration 199)
// and two accounts may hold the same slot; the host guard resolves clashes between the accounts of one computer.
package machineaddress

import "net/netip"

// FromSlot is the address of slot, which must be in [0, 65536).
func FromSlot(slot int) netip.Addr {
	return netip.AddrFrom4([4]byte{127, 100, byte(slot >> 8), byte(slot)})
}
