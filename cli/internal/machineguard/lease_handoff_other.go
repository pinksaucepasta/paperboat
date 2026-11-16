//go:build !linux

package machineguard

// Only the Linux guard keeps its leases across a replacement (lease_handoff_linux.go); elsewhere the daemons
// re-acquire theirs from the new guard.
func leaseHandoff() bool               { return false }
func storeLease(*guardedLease) error   { return nil }
func forgetLease(*guardedLease)        {}
func preservable(*guardedLease) bool   { return false }
func inheritedLeases() []*guardedLease { return nil }
