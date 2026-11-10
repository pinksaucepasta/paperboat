//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// windowsUninstallInFlight is the refusal of a pb install while a pb uninstall helper still removes Paperboat: the
// uninstall command returns at once (state pending) and the helper finishes later; an install started in that gap
// reinstalled the machine guard and roots the helper was deleting and died 'operation_failed / local_io' inside its
// first elevated step, 3 of 3 (winpool-6, spark/install-after-uninstall-diag.md). Failing at once and saying why is the
// pending contract's counterpart: no wait the user did not ask for.
func windowsUninstallInFlight() error {
	root := windowsUninstallRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil // no helper directory: nothing is being removed
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isWindowsUninstallIdentifier(entry.Name()) {
			continue
		}
		directory := filepath.Join(root, entry.Name())
		statusPath := filepath.Join(directory, "status.json")
		status, statusErr := readWindowsUninstallStatus(statusPath)
		if statusErr != nil || (status.State != "scheduled" && status.State != "waiting_for_parent" && status.State != "removing") {
			continue
		}
		running, runErr := windowsHelperIsRunning(filepath.Join(directory, "paperboat-uninstall-helper.exe"))
		if runErr != nil || !running {
			continue // a helper that is gone is the expired-record case: the uninstall recovery cleans it up
		}
		return usageError{
			err:           fmt.Errorf("an uninstall helper is still running (%s)", statusPath),
			publicMessage: fmt.Sprintf("Paperboat is still being removed; run `pb install` again in about a minute (removal status: %s).", statusPath),
			refusal:       true,
		}
	}
	return nil
}
