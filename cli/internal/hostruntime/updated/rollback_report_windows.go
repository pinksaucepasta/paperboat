package updated

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"
)

// windowsRolledBackUpdate outlives the activation journal, which the next staged release
// replaces, so status and doctor still say which update was rolled back (Linux keeps the same
// fields in its idle journal).
type windowsRolledBackUpdate struct {
	PreviousVersion string    `json:"previous_version"`
	FailedVersion   string    `json:"failed_version"`
	Reason          string    `json:"reason"`
	RolledBackAt    time.Time `json:"rolled_back_at"`
}

func windowsRolledBackUpdatePath(stateRoot string) string {
	return filepath.Join(filepath.Dir(windowsActivationJournalPath(stateRoot)), "rolled-back.json")
}

// recordWindowsUpdateOutcome keeps a rolled-back update's record and drops it once an update commits.
func recordWindowsUpdateOutcome(stateRoot string, j windowsActivationJournal, now time.Time) error {
	path := windowsRolledBackUpdatePath(stateRoot)
	switch j.Stage {
	case windowsActivationCommitted:
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	case windowsActivationRolledBack:
		body, err := json.Marshal(windowsRolledBackUpdate{PreviousVersion: j.PreviousVersion, FailedVersion: j.Version, Reason: boundedWindowsActivationFailure(errors.New(j.Failure)), RolledBackAt: now.UTC()})
		if err != nil {
			return err
		}
		return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
	}
	return nil
}

// reportWindowsRolledBackUpdate fills the same status fields as Linux when the running version is
// the one a recorded update rolled back to.
func reportWindowsRolledBackUpdate(stateRoot, activeVersion string, response *ControlResponse) {
	body, err := os.ReadFile(windowsRolledBackUpdatePath(stateRoot))
	if err != nil || len(body) > 8192 {
		return
	}
	var record windowsRolledBackUpdate
	if json.Unmarshal(body, &record) != nil || record.FailedVersion == "" || record.PreviousVersion != activeVersion || response.Activating {
		return
	}
	response.ActivationFailure = "activation_failed"
	response.Transaction.FailedVersion, response.Transaction.FailedStep = record.FailedVersion, "activation"
	response.Transaction.FailedReason, response.Transaction.UpdatedAt = record.Reason, record.RolledBackAt
}
