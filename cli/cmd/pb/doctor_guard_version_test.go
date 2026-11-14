//go:build darwin || linux || windows

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
)

// pb doctor reports a machine guard on another version than pb (the two ship together).
func TestDoctorReportsAMachineGuardOnAnotherVersion(t *testing.T) {
	previous := readMachineGuardVersion
	t.Cleanup(func() { readMachineGuardVersion = previous })
	readMachineGuardVersion = func(context.Context) (string, error) { return buildinfo.Version, nil }
	if check := probeDoctorGuardVersion(context.Background()); check.Status != doctorpkg.StatusPass {
		t.Fatalf("matching versions: %+v", check)
	}
	readMachineGuardVersion = func(context.Context) (string, error) { return "2026.10.23.6", nil }
	check := probeDoctorGuardVersion(context.Background())
	if check.Status != doctorpkg.StatusWarning || !strings.Contains(check.Summary, "2026.10.23.6") || !strings.Contains(check.Summary, buildinfo.Version) {
		t.Fatalf("mismatch: %+v", check)
	}
}

// A guard that never started says why in pb doctor instead of only "could not be asked".
func TestDoctorNamesWhyTheMachineGuardDidNotStart(t *testing.T) {
	prevRead, prevReason := readMachineGuardVersion, machineGuardStartFailure
	t.Cleanup(func() { readMachineGuardVersion, machineGuardStartFailure = prevRead, prevReason })
	readMachineGuardVersion = func(context.Context) (string, error) { return "", errors.New("connect: no such file") }
	machineGuardStartFailure = func() string { return "machine guard could not enable PF: pfctl failed: Operation not permitted" }
	check := probeDoctorGuardVersion(context.Background())
	if check.Status != doctorpkg.StatusWarning || !strings.Contains(check.Summary, "could not enable PF: pfctl failed: Operation not permitted") {
		t.Fatalf("start failure not named: %+v", check)
	}
}
