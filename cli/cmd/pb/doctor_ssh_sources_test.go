package main

import (
	"net/netip"
	"strings"
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
)

// pb doctor warns, with the fix, when the machine guard's managed SSH source addresses are missing (every user's
// managed SSH then shares 127.0.0.1's sshd penalties), and passes when all exist.
func TestDoctorWarnsWhenManagedSSHSourceAddressesAreMissing(t *testing.T) {
	if check := doctorSSHSourcesCheck(func(netip.Addr) bool { return true }); check.Status != doctorpkg.StatusPass || check.Recovery != "" {
		t.Fatalf("all addresses present: %+v", check)
	}
	check := doctorSSHSourcesCheck(func(source netip.Addr) bool { return source.As4()[2] >= 4 })
	if check.Status != doctorpkg.StatusWarning || check.Code != "managed_ssh_sources" || !strings.HasPrefix(check.Summary, "4 of 64 ") || !strings.Contains(check.Recovery, "pb install") {
		t.Fatalf("missing addresses: %+v", check)
	}
}
