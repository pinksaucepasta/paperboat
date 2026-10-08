//go:build windows

package updated

import (
	"context"
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
		c.handoffOnce.Do(func() { close(c.handoff) })
	}
	return result, nil
}
