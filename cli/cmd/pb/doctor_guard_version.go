//go:build darwin || linux || windows

package main

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
)

// probeDoctorGuardVersion reports a machine guard older than pb: the guard ships with pb and every installation,
// update and rollback brings it along. It judges by the installer's own rule (development builds of the same guard
// revision keep their guard), so a dev update that rightly kept the guard is not reported degraded.
func probeDoctorGuardVersion(ctx context.Context) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "machine_guard_version", Status: doctorpkg.StatusPass, Summary: "The machine guard runs this pb's version."}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := doctorDependenciesOf(ctx).guardStatus(ctx)
	if err != nil {
		check.Status, check.Summary = doctorpkg.StatusWarning, "The machine guard could not be asked for its version."
		if reason := doctorDependenciesOf(ctx).guardStartFailure(); reason != "" {
			check.Summary = "The machine guard did not start: " + reason
		}
		check.Recovery = "Run `pb install` again to reinstall the machine guard, then run `pb doctor` again."
		return check
	}
	if !hostinstall.MachineGuardNotOlder(status, buildinfo.Version) {
		check.Status = doctorpkg.StatusWarning
		check.Summary = "The machine guard runs " + status.Version + ", pb is " + buildinfo.Version + "."
		check.Recovery = "Run `pb install` again to install the matching machine guard, then run `pb doctor` again."
	}
	return check
}
