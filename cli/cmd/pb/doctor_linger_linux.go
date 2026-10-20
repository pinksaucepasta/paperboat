//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
)

const (
	doctorSystemdRuntime  = "/run/systemd/system"
	doctorLingerDirectory = "/var/lib/systemd/linger"
)

// doctorLingerProbes warns when systemd would stop this user's services, and so the Paperboat
// daemon, as soon as the last login session ends: a remote machine then drops offline.
func doctorLingerProbes() []doctorpkg.Probe {
	return []doctorpkg.Probe{{Code: "logout_survival", Run: func(context.Context) doctorpkg.Check {
		return doctorLingerCheck(doctorSystemdRuntime, doctorLingerDirectory, currentUserName)
	}}}
}

func currentUserName() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	return account.Username, nil
}

func doctorLingerCheck(systemdRuntime, lingerDirectory string, name func() (string, error)) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "logout_survival"}
	if _, err := os.Stat(systemdRuntime); err != nil {
		check.Status, check.Summary = doctorpkg.StatusPass, "This host does not end services at logout."
		return check
	}
	account, err := name()
	if err != nil {
		check.Status, check.Summary = doctorpkg.StatusUnavailable, "Whether Paperboat keeps running after logout could not be checked."
		check.Recovery = "Run pb doctor again from your normal account."
		return check
	}
	_, err = os.Lstat(filepath.Join(lingerDirectory, account))
	switch {
	case err == nil:
		check.Status, check.Summary = doctorpkg.StatusPass, "Paperboat keeps running after you log out."
	case errors.Is(err, os.ErrNotExist):
		check.Status, check.Summary = doctorpkg.StatusWarning, "Paperboat stops when you log out: lingering is off for "+account+"."
		check.Recovery = "Run sudo loginctl enable-linger " + account + ", or run pb install again."
	default:
		check.Status, check.Summary = doctorpkg.StatusUnavailable, "Whether Paperboat keeps running after logout could not be checked."
		check.Recovery = "Run pb doctor again from your normal account."
	}
	return check
}
