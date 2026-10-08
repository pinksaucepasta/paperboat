package autoupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
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
}

// MaintenanceDue also handles a machine waking after its scheduled time.
// Pending work retries until a verified activation succeeds or is rejected.
func (p Preferences) MaintenanceDue(now time.Time, state MaintenanceState) bool {
	if !p.Enabled || p.Validate() != nil {
		return false
	}
	if state.LocalTime == p.LocalTime && state.Pending {
		return true
	}
	if state.LocalTime == p.LocalTime && state.LocalDate == now.Format("2006-01-02") {
		return false
	}
	return !now.Before(p.maintenanceOn(now))
}

func LoadPreferences(path string, enabled bool) (Preferences, error) {
	var value Preferences
	err := readPrivateJSON(path, &value)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultPreferences(enabled), nil
	}
	if err != nil || value.Validate() != nil {
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
	if errors.Is(err, os.ErrNotExist) {
		return MaintenanceState{}, nil
	}
	if err != nil || state.Schema != "paperboat.update-maintenance/v1" || len(state.LocalDate) != 10 || len(state.LocalTime) != 5 {
		return MaintenanceState{}, ErrInvalidConfig
	}
	if _, err := time.Parse("2006-01-02", state.LocalDate); err != nil {
		return MaintenanceState{}, ErrInvalidConfig
	}
	if _, _, ok := clockTime(state.LocalTime); !ok {
		return MaintenanceState{}, ErrInvalidConfig
	}
	return state, nil
}

func SaveMaintenanceState(path string, p Preferences, now time.Time, pending bool) error {
	if p.Validate() != nil {
		return ErrInvalidConfig
	}
	return writePrivateJSON(path, MaintenanceState{Schema: "paperboat.update-maintenance/v1", LocalDate: now.Format("2006-01-02"), LocalTime: p.LocalTime, Pending: pending})
}

func readPrivateJSON(path string, out any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4096 {
		return ErrInvalidConfig
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return ErrInvalidConfig
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(out) != nil || decoder.Decode(&extra) != io.EOF {
		return ErrInvalidConfig
	}
	return nil
}

func writePrivateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > 4096 || !json.Valid(bytes.TrimSpace(data)) {
		return ErrInvalidConfig
	}
	return atomicfile.Write(path, append(data, '\n'), atomicfile.Options{Mode: 0600, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid()})
}
