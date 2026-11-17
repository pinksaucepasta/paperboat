//go:build darwin || linux || windows

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
)

// pb doctor reports a machine guard on another version than pb (the two ship together).
func TestDoctorReportsAMachineGuardOnAnotherVersion(t *testing.T) {
	previous := readMachineGuardStatus
	t.Cleanup(func() { readMachineGuardStatus = previous })
	readMachineGuardStatus = func(context.Context) (machineguard.RuntimeStatus, error) {
		return machineguard.RuntimeStatus{Version: buildinfo.Version}, nil
	}
	if check := probeDoctorGuardVersion(context.Background()); check.Status != doctorpkg.StatusPass {
		t.Fatalf("matching versions: %+v", check)
	}
	readMachineGuardStatus = func(context.Context) (machineguard.RuntimeStatus, error) {
		return machineguard.RuntimeStatus{Version: "2026.10.23.6"}, nil
	}
	check := probeDoctorGuardVersion(context.Background())
	if check.Status != doctorpkg.StatusWarning || !strings.Contains(check.Summary, "2026.10.23.6") || !strings.Contains(check.Summary, buildinfo.Version) {
		t.Fatalf("mismatch: %+v", check)
	}
}

// A dev update keeps a guard of the same code revision (installer rule); doctor must not call that degraded
// (a Windows test VM 2026-11-16: windows:update FAIL "doctor healthy after the update").
func TestDoctorAcceptsADevGuardOfTheSameRevision(t *testing.T) {
	previous := readMachineGuardStatus
	t.Cleanup(func() { readMachineGuardStatus = previous })
	readMachineGuardStatus = func(context.Context) (machineguard.RuntimeStatus, error) {
		return machineguard.RuntimeStatus{Version: "dev-aaaaaaaaa", Revision: machineguard.Revision}, nil
	}
	if check := probeDoctorGuardVersion(context.Background()); check.Status != doctorpkg.StatusPass {
		t.Fatalf("same revision: %+v", check)
	}
}

// A guard that never started says why in pb doctor instead of only "could not be asked".
func TestDoctorNamesWhyTheMachineGuardDidNotStart(t *testing.T) {
	prevRead, prevReason := readMachineGuardStatus, machineGuardStartFailure
	t.Cleanup(func() { readMachineGuardStatus, machineGuardStartFailure = prevRead, prevReason })
	readMachineGuardStatus = func(context.Context) (machineguard.RuntimeStatus, error) {
		return machineguard.RuntimeStatus{}, errors.New("connect: no such file")
	}
	machineGuardStartFailure = func() string { return "machine guard could not enable PF: pfctl failed: Operation not permitted" }
	check := probeDoctorGuardVersion(context.Background())
	if check.Status != doctorpkg.StatusWarning || !strings.Contains(check.Summary, "could not enable PF: pfctl failed: Operation not permitted") {
		t.Fatalf("start failure not named: %+v", check)
	}
}
