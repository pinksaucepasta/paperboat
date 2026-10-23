//go:build darwin || linux || windows

package main

import (
	"context"
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
