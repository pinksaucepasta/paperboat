package autoupdate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if p.MaintenanceDue(time.Date(2026, 3, 8, 3, 59, 0, 0, zone), MaintenanceState{}) {
		t.Fatal("installed before maintenance")
	}
	wake := time.Date(2026, 3, 8, 11, 0, 0, 0, zone)
	if !p.MaintenanceDue(wake, MaintenanceState{}) {
		t.Fatal("missed window never caught up")
	}
	state := MaintenanceState{LocalDate: "2026-03-08", LocalTime: "04:00"}
	if p.MaintenanceDue(wake, state) {
		t.Fatal("routine installation repeated during daytime")
	}
	state.Pending = true
	if !p.MaintenanceDue(wake, state) {
		t.Fatal("interrupted attempt cannot resume")
	}
	p.Enabled = false
	if p.MaintenanceDue(wake, state) {
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
	if p.MaintenanceDue(second, state) {
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
	if err = SaveMaintenanceState(statePath, p, now, true); err != nil {
		t.Fatal(err)
	}
	state, err := LoadMaintenanceState(statePath)
	if err != nil || !state.Pending || state.LocalDate != "2026-10-08" {
		t.Fatalf("state=%+v %v", state, err)
	}
	if err = os.WriteFile(path, []byte(`{"schema":"paperboat.update-settings/v1","enabled":true,"local_time":"04:00","unknown":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadPreferences(path, true); err == nil {
		t.Fatal("unknown settings silently accepted")
	}
	for _, value := range []string{"4:00", "24:00", "04:60", "-1:00", "aa:bb", "04:00:00"} {
		p.LocalTime = value
		if p.Validate() == nil {
			t.Fatalf("invalid clock %q accepted", value)
		}
	}
}
