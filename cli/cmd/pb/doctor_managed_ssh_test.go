package main

import (
	"errors"
	"strings"
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
)

func TestDoctorManagedSSHCheck(t *testing.T) {
	for _, test := range []struct {
		enrolled bool
		err      error
		state    string
		want     string
	}{
		{true, nil, "not_installed", doctorpkg.StatusFail},
		{true, nil, "active", doctorpkg.StatusPass},
		{true, nil, "installed_inactive", doctorpkg.StatusWarning},
		{false, nil, "not_installed", doctorpkg.StatusPass},
		{true, errors.New("denied"), "active", doctorpkg.StatusUnavailable},
		{true, nil, "unavailable", doctorpkg.StatusUnavailable},
		{true, nil, "invalid", doctorpkg.StatusUnavailable},
	} {
		check := doctorManagedSSHCheck(test.enrolled, test.err, test.state)
		if check.Status != test.want {
			t.Errorf("%v/%v/%s: status %s, want %s", test.enrolled, test.err, test.state, check.Status, test.want)
		}
		if test.state == "not_installed" && test.enrolled && !strings.Contains(check.Recovery, "pb doctor --repair") {
			t.Errorf("missing sshd recovery %q does not name the repair", check.Recovery)
		}
	}
}

func TestManagedSSHInstallWarningOnlyWhenMissing(t *testing.T) {
	if got := managedSSHInstallWarning(doctorManagedSSHCheck(true, nil, "not_installed")); len(got) != 1 || !strings.Contains(got[0], "pb doctor --repair") {
		t.Fatalf("missing sshd warning = %q", got)
	}
	for _, state := range []string{"active", "installed_inactive", "unavailable"} {
		if got := managedSSHInstallWarning(doctorManagedSSHCheck(true, nil, state)); got != nil {
			t.Fatalf("%s: unexpected warning %q", state, got)
		}
	}
}
