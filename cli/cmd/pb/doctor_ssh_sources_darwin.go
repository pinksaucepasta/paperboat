//go:build darwin

package main

import (
	"context"

	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
)

// Only macOS needs the machine guard to alias the managed SSH source addresses; Linux and Windows route all of 127/8.
func doctorSSHSourceProbes() []doctorpkg.Probe {
	return []doctorpkg.Probe{{Code: "managed_ssh_sources", Run: func(context.Context) doctorpkg.Check { return doctorSSHSourcesCheck(loopbackBindable) }}}
}
