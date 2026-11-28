//go:build darwin || linux || windows

package updated

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/autoupdate"
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

// anchorMaintenanceWindow records the window the active version was last kept through when automatic updates are
// switched on or retimed, or when no record for this version exists, so the next window is the first one due (the
// time the command reports as the next maintenance). Without it a machine asleep across its first window found no
// record on wake, read that as a version chosen outside a window and marked the missed window done without
// installing (smoke 1127 AU3/AU3M); and a retime or re-enable after today's window installed at once. Settings that
// change neither (the same time again) keep the record, so a missed window stays due. A failure is logged, not
// returned: settings.json is already saved, and the check records the window itself.
func anchorMaintenanceWindow(root string, previous autoupdate.Preferences, previousKnown bool, settings autoupdate.Preferences, active string, now time.Time) {
	if !settings.Enabled || active == "" {
		return
	}
	path := filepath.Join(root, "maintenance.json")
	state, err := autoupdate.LoadMaintenanceState(path)
	unchanged := previousKnown && previous.Enabled && previous.LocalTime == settings.LocalTime
	if err == nil && state.Active == active && state.LocalTime == settings.LocalTime && (unchanged || state.Pending) {
		return
	}
	if err == nil {
		err = autoupdate.SaveMaintenanceState(path, settings, now.In(machineZone), false, active)
	}
	if err != nil {
		slog.Warn("automatic update window not recorded at settings change", "error", err)
		return
	}
	slog.Info("automatic update window recorded at settings change", "local_time", settings.LocalTime)
}

// logWindowEnded names why an automatic update window ended without installing, so a missed update can be explained
// from the updater's log (smoke 1127 AU3/AU3M had no trace of which path ended the window).
func logWindowEnded(reason string, settings autoupdate.Preferences, version string) {
	slog.Info("automatic update window recorded without installing", "reason", reason, "local_time", settings.LocalTime, "version", version)
}

// machineZone is the machine's local calendar; tests set it instead of time.Local, which every live timer reads.
var machineZone = time.Local

func nextMachineUpdateCheck(root string, enabled bool, now, next time.Time) time.Time {
	settings, err := machineUpdateSettings(root, enabled, nil)
	if err != nil || !settings.Enabled {
		return next
	}
	// Scheduler observations are UTC; scheduling uses the machine local calendar.
	maintenance := settings.NextMaintenance(now.In(machineZone))
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
