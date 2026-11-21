//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"

	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/windowsopenssh"
)

// Windows has no managed SSH source addresses to check (it routes all of 127/8); it checks the sshd service.
func doctorSSHSourceProbes() []doctorpkg.Probe {
	return []doctorpkg.Probe{{Code: "managed_ssh", Run: func(context.Context) doctorpkg.Check { return windowsManagedSSHCheck() }}}
}

// managedSSHInstallWarnings names a missing managed sshd after install or pair: the install skips OpenSSH it
// cannot obtain in time (hostinstall.prepareWindowsSSHWithinAttempts) and reports success otherwise.
func managedSSHInstallWarnings() []string { return managedSSHInstallWarning(windowsManagedSSHCheck()) }

// windowsManagedSSHCheck queries the installed runtime's own sshd generation, PaperboatSshd-<instance>-<sha[:32]>
// (the one its worker serves, production_platform_windows.go), so a retained older generation cannot hide it.
func windowsManagedSSHCheck() doctorpkg.Check {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return doctorManagedSSHCheck(true, err, "", false)
	}
	instance, err := hostinstall.WindowsInstanceForSID(user.User.Sid.String())
	if err != nil {
		return doctorManagedSSHCheck(true, err, "", false)
	}
	installed, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, hostinstall.ErrNotInstalled) || err == nil && installed.EnrollmentPending {
		return doctorManagedSSHCheck(false, nil, "", false)
	}
	if err != nil || len(installed.Source.SHA256) < 32 {
		return doctorManagedSSHCheck(true, errors.Join(errors.New("read installed runtime"), err), "", false)
	}
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return doctorManagedSSHCheck(true, nil, "unavailable", false)
	}
	manager := &mgr.Mgr{Handle: handle}
	defer manager.Disconnect()
	retrying := time.Since(installed.InstalledAt) <= hostinstall.WindowsLateSSHWindow
	return doctorManagedSSHCheck(true, nil, windowsServiceState(manager, windowsopenssh.ServiceName+"-"+instance+"-"+installed.Source.SHA256[:32]), retrying)
}
