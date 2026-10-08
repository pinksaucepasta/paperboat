//go:build darwin || linux || windows

package updated

import (
	"context"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
)

func (c *Client) Settings(ctx context.Context, value *autoupdate.Preferences) (ControlResponse, error) {
	return c.callRequest(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: "settings", Settings: value})
}

func machineUpdateSettings(root string, enabled bool, value *autoupdate.Preferences) (autoupdate.Preferences, error) {
	path := filepath.Join(root, "settings.json")
	if value != nil {
		if err := autoupdate.SavePreferences(path, *value); err != nil {
			return autoupdate.Preferences{}, err
		}
		if !value.Enabled {
			if err := autoupdate.ClearOwnerMaintenance(filepath.Join(root, "owner-maintenance.json")); err != nil {
				return autoupdate.Preferences{}, err
			}
		}
	}
	return autoupdate.LoadPreferences(path, enabled)
}

func nextMachineUpdateCheck(root string, enabled bool, now, next time.Time) time.Time {
	settings, err := machineUpdateSettings(root, enabled, nil)
	if err != nil || !settings.Enabled {
		return next
	}
	maintenance := settings.NextMaintenance(now)
	if maintenance.After(now) && maintenance.Before(next) {
		return maintenance
	}
	return next
}

func populateMachineSettings(response *ControlResponse, root string, enabled bool) error {
	settings, err := machineUpdateSettings(root, enabled, nil)
	if err != nil {
		return err
	}
	response.Settings = &settings
	response.OwnerMaintenance, err = autoupdate.LoadOwnerMaintenance(filepath.Join(root, "owner-maintenance.json"))
	if err != nil {
		return err
	}
	if settings.Enabled {
		response.NextMaintenanceAt = settings.NextMaintenance(time.Now())
	}
	return nil
}
