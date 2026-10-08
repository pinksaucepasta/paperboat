//go:build darwin || linux || windows

package updated

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
)

func TestMachineSettingsPersistAndRespectOptOut(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("device", 19800)
	t.Cleanup(func() { time.Local = previous })
	root := t.TempDir()
	initial, err := machineUpdateSettings(root, true, nil)
	if err != nil || initial != autoupdate.DefaultPreferences(true) {
		t.Fatalf("defaults=%+v err=%v", initial, err)
	}
	initial.Enabled, initial.LocalTime = false, "23:15"
	if _, err := machineUpdateSettings(root, true, &initial); err != nil {
		t.Fatal(err)
	}
	reloaded, err := machineUpdateSettings(root, true, nil)
	if err != nil || reloaded != initial {
		t.Fatalf("restart settings=%+v err=%v", reloaded, err)
	}
	now := time.Date(2026, 10, 8, 22, 0, 0, 0, time.FixedZone("device", 19800))
	nominal := now.Add(3 * time.Hour)
	if got := nextMachineUpdateCheck(root, true, now, nominal); !got.Equal(nominal) {
		t.Fatalf("opt-out changed check: %v", got)
	}
	initial.Enabled = true
	if _, err := machineUpdateSettings(root, true, &initial); err != nil {
		t.Fatal(err)
	}
	want := now.Add(75 * time.Minute)
	if got := nextMachineUpdateCheck(root, true, now, nominal); !got.Equal(want) {
		t.Fatalf("maintenance=%v want=%v", got, want)
	}
	soon := now.Add(5 * time.Minute)
	if got := nextMachineUpdateCheck(root, true, now, soon); !got.Equal(soon) {
		t.Fatalf("maintenance postponed retry: %v", got)
	}
	invalid := initial
	invalid.LocalTime = "24:00"
	if _, err := machineUpdateSettings(root, true, &invalid); err == nil {
		t.Fatal("invalid settings accepted")
	}
	if got, err := machineUpdateSettings(root, false, nil); err != nil || got != initial {
		t.Fatalf("invalid write changed stored settings: %+v %v", got, err)
	}
}

// Scheduler observations use UTC; maintenance still belongs to the machine's
// local calendar, independently of the observation timestamp's location.
func TestMachineMaintenanceWakeUsesLocalClockForUTCObservation(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("IST", 19800)
	t.Cleanup(func() { time.Local = previous })
	root := t.TempDir()
	settings := autoupdate.DefaultPreferences(true)
	if _, err := machineUpdateSettings(root, true, &settings); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 21, 0, 0, 0, time.UTC)
	nominal := now.Add(6 * time.Hour)
	want := time.Date(2026, 10, 8, 22, 30, 0, 0, time.UTC)
	if got := nextMachineUpdateCheck(root, true, now, nominal); !got.Equal(want) {
		t.Fatalf("local 04:00 maintenance wake=%v, want %v", got, want)
	}
}
