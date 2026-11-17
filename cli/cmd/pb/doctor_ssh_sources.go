package main

import (
	"fmt"
	"net"
	"net/netip"

	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"
)

// doctorSSHSourcesCheck reports managed SSH source addresses this computer cannot connect from. The host bridge
// reaches sshd from one loopback address per user (wire.LoopbackSource), so one user's failed logins (OpenSSH 9.8+
// PerSourcePenalties) never block the others; macOS routes only the addresses its machine guard aliases, and without
// them the bridge connects from the shared 127.0.0.1.
func doctorSSHSourcesCheck(bindable func(netip.Addr) bool) doctorpkg.Check {
	sources := wire.LoopbackSources()
	missing := 0
	for _, source := range sources {
		if !bindable(source) {
			missing++
		}
	}
	check := doctorpkg.Check{Category: "local", Code: "managed_ssh_sources", Status: doctorpkg.StatusPass, Summary: "Managed SSH reaches this computer's sshd from a separate loopback address for each user."}
	if missing > 0 {
		check.Status = doctorpkg.StatusWarning
		check.Summary = fmt.Sprintf("%d of %d managed SSH source addresses are missing (the machine guard adds them), so managed SSH from every user shares 127.0.0.1: one user's failed SSH logins can make sshd refuse everyone's managed SSH to this computer for up to 10 minutes.", missing, len(sources))
		check.Recovery = "Run `pb install` again to reinstall the machine guard, then run `pb doctor` again."
	}
	return check
}

func loopbackBindable(source netip.Addr) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort(source.String(), "0"))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}
