//go:build windows

package updated

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
)

func (c *windowsController) automaticCheck(ctx context.Context) (autoupdate.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, maxUpdateControlTimeout)
	defer cancel()
	settings, err := machineUpdateSettings(c.config.StateRoot, c.config.AutomaticChecks, nil)
	if err != nil {
		return autoupdate.Result{}, err
	}
	if !settings.Enabled {
		return c.checkRelease(ctx)
	}
	// A previous SCM start can fail after approval was persisted. Resume the
	// protected transaction rather than attempting a second download/cutover.
	if journal, loadErr := loadWindowsActivationJournalForController(c.config); loadErr == nil {
		if windowsActivationNeedsResume(journal, c.activeVersion, false) {
			err := c.handoffAutomaticActivation(ctx, func(ctx context.Context) error {
				_, err := resumeWindowsActivationForController(ctx, c.config)
				return err
			})
			return autoupdate.Result{Version: journal.Version}, err
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return autoupdate.Result{}, loadErr
	}
	statePath := filepath.Join(c.config.StateRoot, "maintenance.json")
	state, err := autoupdate.LoadMaintenanceState(statePath)
	if err != nil {
		return autoupdate.Result{}, err
	}
	now := time.Now()
	response, err := c.invokeWithAutomatic(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: "download"}, true)
	result := autoupdate.Result{Version: response.Version}
	if err != nil {
		return result, err
	}
	if !settings.MaintenanceDue(now, state) {
		return result, nil
	}
	if response.Candidate == nil {
		return result, autoupdate.SaveMaintenanceState(statePath, settings, now, false)
	}
	if err := autoupdate.SaveMaintenanceState(statePath, settings, now, true); err != nil {
		return result, err
	}
	response, err = c.invokeWithAutomatic(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: "install", ApprovalID: response.Candidate.ID}, true)
	if err != nil {
		return result, err
	}
	result.Version, result.Updated = response.Version, response.Updated
	if err := autoupdate.SaveMaintenanceState(statePath, settings, now, false); err != nil {
		return result, err
	}
	if response.Pending {
		if err := c.handoffAutomaticActivation(ctx, func(context.Context) error {
			err := startWindowsActivator(c.config.OwnerSID)
			if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
				return nil
			}
			return err
		}); err != nil {
			return result, err
		}
	}
	return result, nil
}

// Serialize the final automatic SCM handoff with changes to machine settings.
func (c *windowsController) handoffAutomaticActivation(ctx context.Context, launch func(context.Context) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	settings, err := machineUpdateSettings(c.config.StateRoot, c.config.AutomaticChecks, nil)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return nil
	}
	return c.handoffActivation(ctx, launch)
}
