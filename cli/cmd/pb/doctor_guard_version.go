//go:build darwin || linux || windows

package main

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

// readMachineGuardVersion asks the running machine guard for its version.
var readMachineGuardVersion = func(ctx context.Context) (string, error) {
	client, err := machineguard.Connect(ctx, machineguard.DefaultSocket)
	if err != nil {
		return "", err
	}
	defer client.Close()
	status, err := client.Status(ctx)
	return status.Version, err
}

// probeDoctorGuardVersion reports a machine guard on another version than pb: the guard ships with pb and
// every installation, update and rollback keeps the two equal.
func probeDoctorGuardVersion(ctx context.Context) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "machine_guard_version", Status: doctorpkg.StatusPass, Summary: "The machine guard runs this pb's version."}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	version, err := readMachineGuardVersion(ctx)
	if err != nil {
		check.Status, check.Summary = doctorpkg.StatusWarning, "The machine guard could not be asked for its version."
		check.Recovery = "Run `pb install` again to reinstall the machine guard, then run `pb doctor` again."
		return check
	}
	if version != buildinfo.Version {
		check.Status = doctorpkg.StatusWarning
		check.Summary = "The machine guard runs " + version + ", pb is " + buildinfo.Version + "."
		check.Recovery = "Run `pb install` again to install the matching machine guard, then run `pb doctor` again."
	}
	return check
}
