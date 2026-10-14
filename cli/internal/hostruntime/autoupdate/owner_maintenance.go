package autoupdate

import (
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releasepolicy"
)

const (
	ownerMaintenanceSchema  = "paperboat.owner-maintenance/v1"
	BlockedOwnerMaintenance = "owner_maintenance_pending"
)

// OwnerMaintenanceNotice records a bounded local maintenance window for one
// already-eligible owner-class release candidate.
type OwnerMaintenanceNotice struct {
	Schema      string    `json:"schema"`
	CandidateID string    `json:"candidate_id"`
	Version     string    `json:"version"`
	AnnouncedAt time.Time `json:"announced_at"`
	Deadline    time.Time `json:"deadline"`
}

// OwnerMaintenancePendingError means activation is waiting for the locally
// protected workloads to finish. The retained deadline is not permission to interrupt.
// Its text intentionally omits release
// identifiers and versions.
type OwnerMaintenancePendingError struct {
	Notice OwnerMaintenanceNotice
}

func (OwnerMaintenancePendingError) Error() string {
	return "owner maintenance is pending"
}

// LoadOwnerMaintenance reads the pending local owner-maintenance notice.
func LoadOwnerMaintenance(path string) (*OwnerMaintenanceNotice, error) {
	var notice OwnerMaintenanceNotice
	err := readPrivateJSON(path, &notice)
	if updateCausesOnly(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !validOwnerMaintenanceNotice(notice) {
		return nil, ErrInvalidConfig
	}
	return &notice, nil
}

// PlanOwnerMaintenance persists the first announcement for a candidate. An
// exact retry keeps its original deadline; a replacement candidate inherits
// the earliest pending deadline so repeated releases cannot extend maintenance
// indefinitely.
func PlanOwnerMaintenance(path, candidateID, version string, graceSeconds uint32, now time.Time) (OwnerMaintenanceNotice, error) {
	if !releasepolicy.IsDigest(candidateID) || !releasepolicy.IsVersion(version) ||
		graceSeconds > releasepolicy.MaxRoutineDeferralSec || now.IsZero() {
		return OwnerMaintenanceNotice{}, ErrInvalidConfig
	}

	existing, err := LoadOwnerMaintenance(path)
	if err != nil {
		return OwnerMaintenanceNotice{}, err
	}
	if existing != nil && existing.CandidateID == candidateID {
		if existing.Version != version {
			return OwnerMaintenanceNotice{}, ErrInvalidConfig
		}
		return *existing, nil
	}

	announcedAt := now.UTC()
	deadline := announcedAt.Add(time.Duration(graceSeconds) * time.Second)
	if existing != nil && existing.Deadline.Before(deadline) {
		deadline = existing.Deadline
	}
	notice := OwnerMaintenanceNotice{
		Schema:      ownerMaintenanceSchema,
		CandidateID: candidateID,
		Version:     version,
		AnnouncedAt: announcedAt,
		Deadline:    deadline,
	}
	if !validOwnerMaintenanceNotice(notice) {
		return OwnerMaintenanceNotice{}, ErrInvalidConfig
	}
	if err := writePrivateJSON(path, notice); err != nil {
		return OwnerMaintenanceNotice{}, err
	}
	return notice, nil
}

// ClearOwnerMaintenance removes a pending notice without following a file
// symlink. Missing state is already clear.
func ClearOwnerMaintenance(path string) error {
	parent := filepath.Dir(filepath.Clean(path))
	parentInfo, err := os.Lstat(parent)
	if updateCausesOnly(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return updateFailure("owner maintenance state could not be cleared", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidConfig
	}

	info, err := os.Lstat(path)
	if updateCausesOnly(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return updateFailure("owner maintenance state could not be cleared", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidConfig
	}
	file, err := os.Open(path)
	if err != nil {
		return updateFailure("owner maintenance state could not be cleared", err)
	}
	opened, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return updateFailure("owner maintenance state could not be inspected", statErr, closeErr)
	}
	if !os.SameFile(info, opened) {
		return updateFailure("owner maintenance state is invalid", ErrInvalidConfig, closeErr)
	}
	if closeErr != nil {
		return updateFailure("owner maintenance state could not be closed", closeErr)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return updateFailure("owner maintenance state could not be cleared", err)
	}
	if !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
		return ErrInvalidConfig
	}
	if err := os.Remove(path); err != nil {
		return updateFailure("owner maintenance state could not be cleared", err)
	}
	return nil
}

func validOwnerMaintenanceNotice(notice OwnerMaintenanceNotice) bool {
	if notice.Schema != ownerMaintenanceSchema || !releasepolicy.IsDigest(notice.CandidateID) ||
		!releasepolicy.IsVersion(notice.Version) || notice.AnnouncedAt.IsZero() || notice.Deadline.IsZero() {
		return false
	}
	latest := notice.AnnouncedAt.Add(time.Duration(releasepolicy.MaxRoutineDeferralSec) * time.Second)
	return !notice.Deadline.After(latest)
}
