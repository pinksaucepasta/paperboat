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
	// maxReplacements bounds the note; a computer normally leaves one or two records behind.
	maxReplacements = 32
)

// Replacement is one machine record this computer left behind.
type Replacement struct {
	MachineID string `json:"machine_id"`
	// Unfinished marks a record whose own pairing never finished here (the local installation failed).
	// The next pairing replaces it as soon as it has a record of its own, even if it fails too; a
	// record that worked is replaced only once the new machine works.
	Unfinished bool `json:"unfinished,omitempty"`
}

type replacementMarker struct {
	Schema    string        `json:"schema"`
	Machines  []Replacement `json:"machines"`
	CreatedAt time.Time     `json:"created_at"`
}

// ReplacementPath is where the note lives; pb reset keeps it through its wipe.
func ReplacementPath() (string, error) {
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

// SaveReplacement adds machineID to the records the next pairing of this computer replaces. Records
// already noted stay: a pairing that fails after a reset must not make the note forget the record the
// reset left behind (each failed attempt then left one more record in the account). The server only
// acts on records of the same account.
func SaveReplacement(machineID string, unfinished bool, now time.Time) error {
	if !validReplacementID(machineID) {
		return ErrInvalid
	}
	machines := LoadReplacements(now)
	known := false
	for index := range machines {
		if machines[index].MachineID == machineID {
			machines[index].Unfinished = machines[index].Unfinished || unfinished
			known = true
		}
	}
	if !known {
		machines = append(machines, Replacement{MachineID: machineID, Unfinished: unfinished})
	}
	if len(machines) > maxReplacements {
		// The oldest record holds the computer's name and local address; drop the ones after it.
		machines = append(machines[:1], machines[len(machines)-maxReplacements+1:]...)
	}
	return writeReplacements(machines, now)
}

func writeReplacements(machines []Replacement, createdAt time.Time) error {
	path, err := ReplacementPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(replacementMarker{Schema: replacementSchema, Machines: machines, CreatedAt: createdAt.UTC()})
	if err != nil {
		return err
	}
	return atomicfile.Write(path, encoded, atomicfile.CurrentOwnerOptions(0o600))
}

func loadReplacementMarker(now time.Time) replacementMarker {
	path, err := ReplacementPath()
	if err != nil {
		return replacementMarker{}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return replacementMarker{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return replacementMarker{}
	}
	var marker replacementMarker
	if json.Unmarshal(data, &marker) != nil || marker.Schema != replacementSchema || len(marker.Machines) > maxReplacements {
		return replacementMarker{}
	}
	for _, machine := range marker.Machines {
		if !validReplacementID(machine.MachineID) {
			return replacementMarker{}
		}
	}
	if age := now.Sub(marker.CreatedAt); age < -time.Minute || age > replacementLifetime {
		return replacementMarker{}
	}
	return marker
}

// LoadReplacements returns the records the next pairing replaces, oldest first; none when no note is
// pending (absent, unreadable, malformed or older than a day).
func LoadReplacements(now time.Time) []Replacement {
	return loadReplacementMarker(now).Machines
}

// LoadReplacement returns the record this computer was last, which the pairing request names, or "".
func LoadReplacement(now time.Time) string {
	machines := LoadReplacements(now)
	if len(machines) == 0 {
		return ""
	}
	return machines[len(machines)-1].MachineID
}

// ForgetReplacement drops one record from the note once it has been replaced.
func ForgetReplacement(machineID string, now time.Time) error {
	marker := loadReplacementMarker(now)
	kept := make([]Replacement, 0, len(marker.Machines))
	for _, machine := range marker.Machines {
		if machine.MachineID != machineID {
			kept = append(kept, machine)
		}
	}
	if len(kept) == 0 {
		return ClearReplacement()
	}
	return writeReplacements(kept, marker.CreatedAt)
}

// ClearReplacement removes the pending note once the new machine replaced the old record.
func ClearReplacement() error {
	path, err := ReplacementPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
