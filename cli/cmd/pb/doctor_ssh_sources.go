package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"time"

	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
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

// doctorSSHPortCheck reports a port set with `pb config set ssh-target-port` that no SSH server answers on. Every pb ssh
// to this computer needs one; an unset or default port is the readiness check's business.
func doctorSSHPortCheck(ctx context.Context) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "managed_ssh_port", Status: doctorpkg.StatusPass, Summary: "The configured SSH port is in use."}
	port, set := configuredSSHTargetPort()
	if !set || port == 22 || runtime.GOOS == "windows" { // Windows serves its installed sshd generation's port, not this one
		check.Summary = "Managed SSH uses the default port."
		return check
	}
	if _, err := managedssh.ProbeLoopbackSSH(ctx, port, 3*time.Second); err != nil {
		check.Status = doctorpkg.StatusWarning
		check.Summary = fmt.Sprintf("No SSH server answers on port %d of this computer, the port Paperboat is set to use for pb ssh.", port)
		check.Recovery = "Start the SSH server on that port, or set the right one with `pb config set ssh-target-port <port>`."
	}
	return check
}
