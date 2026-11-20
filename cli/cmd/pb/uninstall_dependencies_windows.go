//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
)

// windowsUninstallDependencies are the edges the Windows uninstall and install-in-flight checks reach outside
// the process: the running executable, process queries, the helper's status file and the user's temp and RunOnce
// locations. Tests replace them to run against a private directory and registry key.
type windowsUninstallDependencies struct {
	executable       func() (string, error)
	processIsRunning func(uint32) (bool, error)
	helperIsRunning  func(string) (bool, error)
	readStatus       func(string) (windowsUninstallStatus, error)
	root             func() string
	// runOnceKey is the user's own list of commands Windows runs once at that user's next sign-in.
	runOnceKey string
	// afterUserStateCheck runs between the path check and the removal; tests plant a link there.
	afterUserStateCheck func()
	// removeStateElevated is removeWindowsStateByHandle; tests replace it to prove that a removal needed only
	// the user's rights.
	removeStateElevated func(path string, preserved []string) error
}

func defaultWindowsUninstallDependencies() windowsUninstallDependencies {
	return windowsUninstallDependencies{
		executable:          os.Executable,
		processIsRunning:    isWindowsProcessRunning,
		helperIsRunning:     windowsExecutableIsRunning,
		readStatus:          readWindowsUninstallStatus,
		root:                func() string { return filepath.Join(os.TempDir(), "Paperboat Uninstall") },
		runOnceKey:          `Software\Microsoft\Windows\CurrentVersion\RunOnce`,
		afterUserStateCheck: func() {},
		removeStateElevated: removeWindowsStateByHandle,
	}
}

func windowsUninstallDependenciesOf(ctx context.Context) windowsUninstallDependencies {
	return dependenciesIn(ctx, defaultWindowsUninstallDependencies)
}
