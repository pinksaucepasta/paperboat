package bootstrap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

const (
	replacementSchema = "paperboat.pending-replacement/v1"
	// replacementLifetime bounds how long a reset's note stays valid: it is only a request for the
	// next pairing of the same computer, never authority.
	replacementLifetime = 24 * time.Hour
)

type replacementMarker struct {
	Schema    string    `json:"schema"`
	MachineID string    `json:"machine_id"`
	CreatedAt time.Time `json:"created_at"`
}

func replacementPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "paperboat", "replaces-machine.json"), nil
}

func validReplacementID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	return !strings.ContainsAny(value, "\x00\r\n\t /\\")
}

// SaveReplacement records, after a reset wiped this computer's state, which machine record the
// next pairing replaces. The server treats it as a request: it only honours a retired record of
// the same account.
func SaveReplacement(machineID string, now time.Time) error {
	if !validReplacementID(machineID) {
		return ErrInvalid
	}
	path, err := replacementPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(replacementMarker{Schema: replacementSchema, MachineID: machineID, CreatedAt: now.UTC()})
	if err != nil {
		return err
	}
	return atomicfile.Write(path, encoded, atomicfile.CurrentOwnerOptions(0o600))
}

// LoadReplacement returns the machine ID the next pairing replaces, or "" when none is pending
// (absent, unreadable, malformed or older than a day).
func LoadReplacement(now time.Time) string {
	path, err := replacementPath()
	if err != nil {
		return ""
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var marker replacementMarker
	if json.Unmarshal(data, &marker) != nil || marker.Schema != replacementSchema || !validReplacementID(marker.MachineID) {
		return ""
	}
	if age := now.Sub(marker.CreatedAt); age < -time.Minute || age > replacementLifetime {
		return ""
	}
	return marker.MachineID
}

// ClearReplacement removes the pending note once a pairing carried it.
func ClearReplacement() error {
	path, err := replacementPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
