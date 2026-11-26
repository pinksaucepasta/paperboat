package autoupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"
)

func TestMaintenanceUsesLocalCalendarAndCatchesMissedWindow(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultPreferences(true)
	before := time.Date(2026, 3, 7, 20, 0, 0, 0, zone)
	next := p.NextMaintenance(before)
	if next.Day() != 8 || next.Hour() != 4 || next.Sub(before) != 7*time.Hour {
		t.Fatalf("DST next=%v delay=%v", next, next.Sub(before))
	}
	if p.MaintenanceDue(time.Date(2026, 3, 8, 3, 59, 0, 0, zone), MaintenanceState{}, "") {
		t.Fatal("installed before maintenance")
	}
	wake := time.Date(2026, 3, 8, 11, 0, 0, 0, zone)
	if !p.MaintenanceDue(wake, MaintenanceState{}, "") {
		t.Fatal("missed window never caught up")
	}
	state := MaintenanceState{LocalDate: "2026-03-08", LocalTime: "04:00"}
	if p.MaintenanceDue(wake, state, "") {
		t.Fatal("routine installation repeated during daytime")
	}
	state.Pending = true
	if !p.MaintenanceDue(wake, state, "") {
		t.Fatal("interrupted attempt cannot resume")
	}
	p.Enabled = false
	if p.MaintenanceDue(wake, state, "") {
		t.Fatal("opt-out ignored")
	}
}

func TestMaintenanceSkippedAndRepeatedWallTimes(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultPreferences(true)
	p.LocalTime = "02:30"
	next := p.NextMaintenance(time.Date(2026, 3, 8, 1, 0, 0, 0, zone))
	if next.Hour() != 3 || next.Minute() != 0 || next.Day() != 8 {
		t.Fatalf("skipped wall time=%v", next)
	}
	p.LocalTime = "01:30"
	first := p.NextMaintenance(time.Date(2026, 11, 1, 0, 0, 0, 0, zone))
	second := first.Add(time.Hour)
	state := MaintenanceState{LocalDate: "2026-11-01", LocalTime: "01:30"}
	if p.MaintenanceDue(second, state, "") {
		t.Fatal("fall-back repeated update")
	}
}

func TestMachinePreferencesAndMaintenanceSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	p, err := LoadPreferences(path, true)
	if err != nil || !p.Enabled || p.LocalTime != "04:00" {
		t.Fatalf("default=%+v %v", p, err)
	}
	p.Enabled = false
	p.LocalTime = "23:45"
	if err = SavePreferences(path, p); err != nil {
		t.Fatal(err)
	}
	p, err = LoadPreferences(path, true)
	if err != nil || p.Enabled || p.LocalTime != "23:45" {
		t.Fatalf("opt-out=%+v %v", p, err)
	}
	statePath := filepath.Join(filepath.Dir(path), "maintenance.json")
	now := time.Date(2026, 10, 8, 23, 46, 0, 0, time.FixedZone("local", 19800))
	if err = SaveMaintenanceState(statePath, p, now, true, ""); err != nil {
		t.Fatal(err)
	}
	state, err := LoadMaintenanceState(statePath)
	if err != nil || !state.Pending || state.LocalDate != "2026-10-08" {
		t.Fatalf("state=%+v %v", state, err)
	}
	if err = os.WriteFile(path, []byte(`{"schema":"paperboat.update-settings/v1","enabled":true,"local_time":"04:00","unknown":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	// A newer build may have written a field before an update rolled back to this one.
	if _, err = LoadPreferences(path, true); err != nil {
		t.Fatalf("settings with a newer field refused: %v", err)
	}
	for _, value := range []string{"4:00", "24:00", "04:60", "-1:00", "aa:bb", "04:00:00"} {
		p.LocalTime = value
		if p.Validate() == nil {
			t.Fatalf("invalid clock %q accepted", value)
		}
	}
}

func TestMachinePreferencesPreserveNativeAndDecodeFailuresAndRecover(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "private-location")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPreferences(filepath.Join(blocker, "settings.json"), true); !errors.Is(err, atomicfile.ErrNotDirectory) || strings.Contains(err.Error(), blocker) {
		t.Fatal("native failure lost or private path exposed")
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"schema":"paperboat.update-settings/v1","enabled":"private-value","local_time":"04:00"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPreferences(path, true)
	var decode *json.UnmarshalTypeError
	if !errors.Is(err, ErrInvalidConfig) || !errors.As(err, &decode) || strings.Contains(err.Error(), "private-value") {
		t.Fatal("original parser cause lost or payload exposed")
	}
	if err := SavePreferences(path, DefaultPreferences(true)); err != nil {
		t.Fatal(err)
	}
	if p, err := LoadPreferences(path, false); err != nil || !p.Enabled {
		t.Fatal("fresh protected preferences did not recover")
	}
}

// A version chosen by install or manual pb update stays until the next window (mp5, smoke 1126:
// .21 installed 46 s after a manual update at 20:20); a machine asleep at its window still catches up.
func TestChosenVersionWaitsForNextWindow(t *testing.T) {
	p := DefaultPreferences(true)
	path := filepath.Join(t.TempDir(), "maintenance.json")
	zone := time.FixedZone("local", 0)
	evening := time.Date(2026, 11, 26, 20, 21, 0, 0, zone)
	// Fresh install: no state, other executable.
	if due, err := p.Due(path, evening, MaintenanceState{}, "new"); due || err != nil {
		t.Fatalf("fresh install due=%v %v", due, err)
	}
	state, err := LoadMaintenanceState(path)
	if err != nil || state.Active != "new" || state.LocalDate != "2026-11-26" {
		t.Fatalf("state=%+v %v", state, err)
	}
	if due, _ := p.Due(path, evening.Add(time.Minute), state, "new"); due {
		t.Fatal("chosen version updated before the next window")
	}
	if due, _ := p.Due(path, time.Date(2026, 11, 27, 4, 0, 0, 0, zone), state, "new"); !due {
		t.Fatal("next window not due")
	}
	// Asleep through the window, woken later the same day: catches up.
	if due, _ := p.Due(path, time.Date(2026, 11, 27, 9, 0, 0, 0, zone), state, "new"); !due {
		t.Fatal("missed window does not catch up on wake")
	}
	// A manual update before today's window counts for yesterday's only: today's still runs.
	early := time.Date(2026, 11, 28, 2, 0, 0, 0, zone)
	if due, _ := p.Due(path, early, state, "manual"); due {
		t.Fatal("manual update before the window updated at once")
	}
	state, _ = LoadMaintenanceState(path)
	if due, _ := p.Due(path, early.Add(2*time.Hour), state, "manual"); !due || state.LocalDate != "2026-11-27" {
		t.Fatalf("today's window skipped after an early manual update: %+v", state)
	}
}

// A version chosen while an automatic attempt was pending replaces that attempt (review finding).
func TestChosenVersionReplacesPendingAttempt(t *testing.T) {
	p := DefaultPreferences(true)
	path := filepath.Join(t.TempDir(), "maintenance.json")
	at := time.Date(2026, 11, 26, 20, 0, 0, 0, time.UTC)
	pending := MaintenanceState{Schema: "paperboat.update-maintenance/v1", LocalDate: "2026-11-26", LocalTime: "04:00", Pending: true, Active: "old"}
	if due, _ := p.Due(path, at, pending, "old"); !due {
		t.Fatal("pending attempt on the same executable not retried")
	}
	if due, err := p.Due(path, at, pending, "manual"); due || err != nil {
		t.Fatalf("manual version replaced at once: %v", err)
	}
	if state, _ := LoadMaintenanceState(path); state.Pending || state.Active != "manual" {
		t.Fatalf("state=%+v", state)
	}
}
