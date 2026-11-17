//go:build darwin || linux || windows

package main

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
)

// readMachineGuardStatus asks the running machine guard for its status (version and code revision).
var readMachineGuardStatus = func(ctx context.Context) (machineguard.RuntimeStatus, error) {
	client, err := machineguard.Connect(ctx, machineguard.DefaultSocket)
	if err != nil {
		return machineguard.RuntimeStatus{}, err
	}
	defer client.Close()
	return client.Status(ctx)
}

// machineGuardStartFailure is the reason the guard last stopped before serving; tests replace it.
var machineGuardStartFailure = machineguard.StartFailure

// probeDoctorGuardVersion reports a machine guard older than pb: the guard ships with pb and every installation,
// update and rollback brings it along. It judges by the installer's own rule (development builds of the same guard
// revision keep their guard), so a dev update that rightly kept the guard is not reported degraded.
func probeDoctorGuardVersion(ctx context.Context) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "machine_guard_version", Status: doctorpkg.StatusPass, Summary: "The machine guard runs this pb's version."}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := readMachineGuardStatus(ctx)
	if err != nil {
		check.Status, check.Summary = doctorpkg.StatusWarning, "The machine guard could not be asked for its version."
		if reason := machineGuardStartFailure(); reason != "" {
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
