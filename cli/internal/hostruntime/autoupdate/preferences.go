package autoupdate

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"
)

const PreferencesSchema = "paperboat.update-settings/v1"

// Preferences belong to the machine, independently of the selected account.
type Preferences struct {
	Schema    string `json:"schema"`
	Enabled   bool   `json:"enabled"`
	LocalTime string `json:"local_time"`
}

func DefaultPreferences(enabled bool) Preferences {
	return Preferences{Schema: PreferencesSchema, Enabled: enabled, LocalTime: "04:00"}
}

func (p Preferences) Validate() error {
	if p.Schema != PreferencesSchema || len(p.LocalTime) != 5 || p.LocalTime[2] != ':' {
		return ErrInvalidConfig
	}
	hour, minute, ok := clockTime(p.LocalTime)
	if !ok || hour > 23 || minute > 59 {
		return ErrInvalidConfig
	}
	return nil
}

func clockTime(value string) (int, int, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, 0, false
	}
	for _, part := range parts {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, 0, false
			}
		}
	}
	hour, _ := strconv.Atoi(parts[0])
	minute, _ := strconv.Atoi(parts[1])
	return hour, minute, hour < 24 && minute < 60
}

// NextMaintenance uses calendar days, so DST changes do not turn a local
// maintenance time into a fixed UTC interval. A skipped wall time runs at the
// first valid minute after it; a repeated wall time runs once per local date.
func (p Preferences) NextMaintenance(now time.Time) time.Time {
	if p.Validate() != nil {
		return time.Time{}
	}
	start := p.maintenanceOn(now)
	if !start.After(now) {
		start = p.maintenanceOn(now.AddDate(0, 0, 1))
	}
	return start
}

func (p Preferences) maintenanceOn(day time.Time) time.Time {
	hour, minute, _ := clockTime(p.LocalTime)
	year, month, date := day.Date()
	// Search the actual day's minutes rather than assuming every wall time
	// exists or that a civil day always contains 24 hours.
	start := time.Date(year, month, date, 0, 0, 0, 0, day.Location())
	end := start.AddDate(0, 0, 1)
	for current := start; current.Before(end); current = current.Add(time.Minute) {
		h, m, _ := current.Clock()
		if h > hour || h == hour && m >= minute {
			return current
		}
	}
	return end
}

type MaintenanceState struct {
	Schema    string `json:"schema"`
	LocalDate string `json:"local_date"`
	LocalTime string `json:"local_time"`
	Pending   bool   `json:"pending"`
	// Active is the SHA-256 of the executable running when the state was saved. Another one means an
	// install or a manual pb update chose the version outside a window: it stays until the next window.
	Active string `json:"active"`
}

// MaintenanceDue also handles a machine waking after its scheduled time.
// Pending work retries until a verified activation succeeds or is rejected.
// A version chosen outside a window (state.Active differs from active) is not due:
// Due records that choice as the latest window's run.
func (p Preferences) MaintenanceDue(now time.Time, state MaintenanceState, active string) bool {
	if !p.Enabled || p.Validate() != nil {
		return false
	}
	// Checked before Pending: a version chosen while an automatic attempt was pending replaces that attempt.
	if state.Active != active {
		return false
	}
	if state.LocalTime == p.LocalTime && state.Pending {
		return true
	}
	if state.LocalTime == p.LocalTime && state.LocalDate == p.latestWindow(now).Format("2006-01-02") {
		return false
	}
	return !now.Before(p.maintenanceOn(now))
}

// Due is MaintenanceDue that also records a version chosen outside a window, so the
// machine keeps it until the next window instead of updating at once (smoke 1126 mp5).
func (p Preferences) Due(path string, now time.Time, state MaintenanceState, active string) (bool, error) {
	if p.MaintenanceDue(now, state, active) {
		return true, nil
	}
	if p.Enabled && state.Active != active {
		return false, SaveMaintenanceState(path, p, now, false, active)
	}
	return false, nil
}

// latestWindow is the start of the most recent window at or before now.
func (p Preferences) latestWindow(now time.Time) time.Time {
	if start := p.maintenanceOn(now); !start.After(now) {
		return start
	}
	return p.maintenanceOn(now.AddDate(0, 0, -1))
}

func LoadPreferences(path string, enabled bool) (Preferences, error) {
	var value Preferences
	err := readPrivateJSON(path, &value)
	if updateCausesOnly(err, os.ErrNotExist) {
		return DefaultPreferences(enabled), nil
	}
	if err != nil {
		return Preferences{}, err
	}
	if value.Validate() != nil {
		return Preferences{}, ErrInvalidConfig
	}
	return value, nil
}

func SavePreferences(path string, value Preferences) error {
	if err := value.Validate(); err != nil {
		return err
	}
	return writePrivateJSON(path, value)
}

func LoadMaintenanceState(path string) (MaintenanceState, error) {
	var state MaintenanceState
	err := readPrivateJSON(path, &state)
	if updateCausesOnly(err, os.ErrNotExist) {
		return MaintenanceState{}, nil
	}
	if err != nil {
		return MaintenanceState{}, err
	}
	if state.Schema != "paperboat.update-maintenance/v1" || len(state.LocalDate) != 10 || len(state.LocalTime) != 5 {
		return MaintenanceState{}, ErrInvalidConfig
	}
	if _, err := time.Parse("2006-01-02", state.LocalDate); err != nil {
		return MaintenanceState{}, updateFailure("automatic update maintenance state is invalid", ErrInvalidConfig, err)
	}
	if _, _, ok := clockTime(state.LocalTime); !ok {
		return MaintenanceState{}, ErrInvalidConfig
	}
	return state, nil
}

// SaveMaintenanceState records the latest window at or before now as run, with the active executable.
func SaveMaintenanceState(path string, p Preferences, now time.Time, pending bool, active string) error {
	if p.Validate() != nil {
		return ErrInvalidConfig
	}
	return writePrivateJSON(path, MaintenanceState{Schema: "paperboat.update-maintenance/v1", LocalDate: p.latestWindow(now).Format("2006-01-02"), LocalTime: p.LocalTime, Pending: pending, Active: active})
}

func readPrivateJSON(path string, out any) (result error) {
	info, err := os.Lstat(path)
	if err = atomicfile.AbsentOrBlocked(path, err); err != nil {
		return updateFailure("automatic update state could not be inspected", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4096 {
		return ErrInvalidConfig
	}
	file, err := os.Open(path)
	if err != nil {
		return updateFailure("automatic update state could not be opened", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			result = updateFailure("automatic update state could not be closed", result, err)
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return updateFailure("automatic update state could not be inspected", err)
	}
	if !os.SameFile(info, opened) {
		return ErrInvalidConfig
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return updateFailure("automatic update state could not be read", err)
	}
	if len(raw) > 4096 {
		return ErrInvalidConfig
	}
	// A newer build may have written this before an update rolled back to this one: ignore its fields.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(out); err != nil {
		return updateFailure("automatic update state is invalid", ErrInvalidConfig, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return updateFailure("automatic update state is invalid", ErrInvalidConfig, err)
	}
	return nil
}

func writePrivateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return updateFailure("automatic update state could not be encoded", ErrInvalidConfig, err)
	}
	if len(data) > 4096 || !json.Valid(bytes.TrimSpace(data)) {
		return ErrInvalidConfig
	}
	if err := atomicfile.Write(path, append(data, '\n'), atomicfile.CurrentOwnerOptions(0600)); err != nil {
		return updateFailure("automatic update state could not be saved", err)
	}
	return nil
}
