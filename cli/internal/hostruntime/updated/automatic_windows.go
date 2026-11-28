//go:build windows

package updated

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
)

func (c *windowsController) automaticCheck(ctx context.Context) (autoupdate.Result, error) {
	if c.config.Source.Distribution == installsource.Custom {
		return autoupdate.Result{Version: c.activeVersion}, nil
	}
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
				resumed, err := resumeWindowsActivationForController(ctx, c.config)
				if err == nil && !resumed {
					return errActivatorOwnsTransaction
				}
				return err
			})
			// A running activator owns the transaction: this updater keeps its pipe and answers status (activating)
			// through the cutover and health check instead of exiting (winpool-9 2026-11-26: down for 10 minutes).
			if errors.Is(err, errActivatorOwnsTransaction) {
				err = nil
			}
			return autoupdate.Result{Version: journal.Version}, err
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return autoupdate.Result{}, loadErr
	}
	statePath := filepath.Join(c.config.StateRoot, "maintenance.json")
	response, err := c.invokeWithAutomatic(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: "download"}, true)
	result := autoupdate.Result{Version: response.Version}
	if err != nil {
		return result, err
	}
	// The download can take a while and settings change meanwhile: decide on the settings as they are now (as Unix).
	if settings, err = machineUpdateSettings(c.config.StateRoot, c.config.AutomaticChecks, nil); err != nil {
		return result, err
	}
	if !settings.Enabled {
		return result, nil
	}
	active := c.config.Source.SHA256
	now := time.Now() // after the download, which can take a while: the window is decided and recorded on one clock
	// Read after the download too: a settings change meanwhile records the window it was kept through.
	state, err := autoupdate.LoadMaintenanceState(statePath)
	if err != nil {
		return result, err
	}
	if due, err := settings.Due(statePath, now, state, active); !due || err != nil {
		return result, err
	}
	if response.Candidate == nil {
		logWindowEnded("no candidate (no update available, or the installation is not committed yet)", settings, result.Version)
		return result, autoupdate.SaveMaintenanceState(statePath, settings, now, false, active)
	}
	if err := autoupdate.SaveMaintenanceState(statePath, settings, now, true, active); err != nil {
		return result, err
	}
	response, err = c.invokeWithAutomatic(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: "install", ApprovalID: response.Candidate.ID}, true)
	if err != nil {
		return result, err
	}
	result.Version, result.Updated = response.Version, response.Updated
	if !response.Updated && !response.Pending {
		logWindowEnded("activation did not start", settings, response.Version)
	}
	if err := autoupdate.SaveMaintenanceState(statePath, settings, now, false, active); err != nil {
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

// errActivatorOwnsTransaction stops a handoff when no activator was started because one already runs.
var errActivatorOwnsTransaction = errors.New("the update activator already owns the transaction")

// Serialize the final automatic SCM handoff with changes to machine settings.
func (c *windowsController) handoffAutomaticActivation(ctx context.Context, launch func(context.Context) error) error {
	if c.config.Source.Distribution == installsource.Custom {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	settings, err := machineUpdateSettings(c.config.StateRoot, c.config.AutomaticChecks, nil)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		logWindowEnded("automatic updates switched off before the activation started", settings, "")
		return nil
	}
	return c.handoffActivation(ctx, launch)
}
