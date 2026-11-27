//go:build darwin || linux || windows

package updated

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/autoupdate"
)

// Smoke 1127 AU3/AU3M: automatic updates switched on two minutes before the window, the updater asleep across it,
// and no maintenance record yet. On wake the window must still be due, not recorded as done without an install.
func TestMissedFirstWindowStaysDueAfterSettingsChange(t *testing.T) {
	previous := machineZone
	machineZone = time.UTC
	t.Cleanup(func() { machineZone = previous })
	root := t.TempDir()
	path := filepath.Join(root, "maintenance.json")
	settings := autoupdate.Preferences{Schema: "paperboat.update-settings/v1", Enabled: true, LocalTime: "20:58"}
	changed := time.Date(2026, 11, 27, 20, 56, 50, 0, time.UTC)
	woke := time.Date(2026, 11, 27, 20, 58, 33, 0, time.UTC)
	const active = "3fac666a1705b694e78808d8d5d5b8f73e9d52bf0574ac1d4863fda9c2acfaec"

	anchorMaintenanceWindow(root, autoupdate.DefaultPreferences(true), true, settings, active, changed)
	state, err := autoupdate.LoadMaintenanceState(path)
	if err != nil {
		t.Fatal(err)
	}
	due, err := settings.Due(path, woke, state, active)
	if err != nil || !due {
		t.Fatalf("missed window after wake: due=%v err=%v, want due", due, err)
	}
	// A pending attempt for this version and time survives another settings change.
	if err := autoupdate.SaveMaintenanceState(path, settings, woke, true, active); err != nil {
		t.Fatal(err)
	}
	pending, _ := autoupdate.LoadMaintenanceState(path)
	anchorMaintenanceWindow(root, autoupdate.DefaultPreferences(true), true, settings, active, woke.Add(time.Minute))
	if again, _ := autoupdate.LoadMaintenanceState(path); again != pending {
		t.Fatalf("pending attempt rewritten: %+v -> %+v", pending, again)
	}
}

// Without a record the wake reads as a version chosen outside a window: this is the path the anchor removes.
func TestMissedFirstWindowWithoutRecordIsConsumed(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "maintenance.json")
	settings := autoupdate.Preferences{Schema: "paperboat.update-settings/v1", Enabled: true, LocalTime: "20:58"}
	woke := time.Date(2026, 11, 27, 20, 58, 33, 0, time.Local)
	if due, err := settings.Due(path, woke, autoupdate.MaintenanceState{}, "abc"); err != nil || due {
		t.Fatalf("due=%v err=%v: expected the pre-fix behaviour this anchor guards against", due, err)
	}
}

// Disabled settings or an unknown active digest record nothing.
func TestAnchorMaintenanceWindowSkipsDisabled(t *testing.T) {
	root := t.TempDir()
	settings := autoupdate.Preferences{Schema: "paperboat.update-settings/v1", Enabled: false, LocalTime: "04:00"}
	anchorMaintenanceWindow(root, settings, true, settings, "abc", time.Now())
	if state, err := autoupdate.LoadMaintenanceState(filepath.Join(root, "maintenance.json")); err != nil || state.Schema != "" {
		t.Fatalf("disabled settings wrote %+v (%v)", state, err)
	}
}

// Retimed (or re-enabled) after the new time has passed today: the next window is tomorrow, as the command reports,
// not an install at once (Hydrogen review of the first anchor).
func TestRetimeAfterTodaysWindowWaitsForTomorrow(t *testing.T) {
	previous := machineZone
	machineZone = time.UTC
	t.Cleanup(func() { machineZone = previous })
	root := t.TempDir()
	path := filepath.Join(root, "maintenance.json")
	const active = "abc"
	old := autoupdate.Preferences{Schema: "paperboat.update-settings/v1", Enabled: true, LocalTime: "04:00"}
	if err := autoupdate.SaveMaintenanceState(path, old, time.Date(2026, 11, 26, 5, 0, 0, 0, time.UTC), false, active); err != nil {
		t.Fatal(err)
	}
	retimed := old
	retimed.LocalTime = "09:00"
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	anchorMaintenanceWindow(root, old, true, retimed, active, now)
	state, err := autoupdate.LoadMaintenanceState(path)
	if err != nil {
		t.Fatal(err)
	}
	if due, err := retimed.Due(path, now.Add(time.Second), state, active); err != nil || due {
		t.Fatalf("retime to a passed time: due=%v err=%v, want not due until tomorrow", due, err)
	}
	if due, _ := retimed.Due(path, time.Date(2026, 11, 28, 9, 0, 30, 0, time.UTC), state, active); !due {
		t.Fatal("tomorrow's window not due after a retime")
	}
}

// The same settings again after a missed window (e.g. a script re-applying them) keep that window due (Hydrogen
// review of the second anchor); switching on again from off records it, as a re-enable after the window.
func TestUnchangedSettingsKeepMissedWindowDue(t *testing.T) {
	previous := machineZone
	machineZone = time.UTC
	t.Cleanup(func() { machineZone = previous })
	root := t.TempDir()
	path := filepath.Join(root, "maintenance.json")
	const active = "abc"
	settings := autoupdate.Preferences{Schema: "paperboat.update-settings/v1", Enabled: true, LocalTime: "04:00"}
	if err := autoupdate.SaveMaintenanceState(path, settings, time.Date(2026, 11, 26, 5, 0, 0, 0, time.UTC), false, active); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC) // slept across today's 04:00
	anchorMaintenanceWindow(root, settings, true, settings, active, now)
	state, _ := autoupdate.LoadMaintenanceState(path)
	if due, err := settings.Due(path, now, state, active); err != nil || !due {
		t.Fatalf("unchanged settings cancelled the missed window: due=%v err=%v", due, err)
	}
	off := settings
	off.Enabled = false
	anchorMaintenanceWindow(root, off, true, settings, active, now)
	state, _ = autoupdate.LoadMaintenanceState(path)
	if due, _ := settings.Due(path, now, state, active); due {
		t.Fatal("re-enabled after today's window: installs at once")
	}
}
