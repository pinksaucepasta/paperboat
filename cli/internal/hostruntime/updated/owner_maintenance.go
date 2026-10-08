//go:build darwin || linux || windows

package updated

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

type maintenanceOwner interface {
	Active(context.Context) (hostdproto.Status, error)
	PrepareMaintenance(context.Context, bool) error
}

// Planning announces an interruption without taking the short-lived admission
// lease. The lease is acquired only by the actual activation transaction.
func planOwnerMaintenance(ctx context.Context, root string, owner maintenanceOwner, release workerupdate.Release, manual bool) error {
	if !release.SupervisorMaintenance || manual {
		return nil
	}
	status, err := owner.Active(ctx)
	if err != nil {
		return err
	}
	if status.ProtectedWorkloads == 0 {
		return nil
	}
	return pendingOwnerMaintenance(root, release)
}

func pendingOwnerMaintenance(root string, release workerupdate.Release) error {
	candidate, err := workerupdate.PreparedCandidateForRelease(release)
	if err != nil {
		return err
	}
	notice, err := autoupdate.PlanOwnerMaintenance(filepath.Join(root, "owner-maintenance.json"), candidate.ID, release.Version, release.OwnerMaintenanceGraceSeconds, time.Now())
	if err != nil {
		return err
	}
	if time.Now().Before(notice.Deadline) {
		return &autoupdate.OwnerMaintenancePendingError{Notice: notice}
	}
	return nil
}

func authorizeOwnerMaintenance(ctx context.Context, root string, owner maintenanceOwner, release workerupdate.Release, manual bool) error {
	if !release.SupervisorMaintenance {
		return nil
	}
	if err := planOwnerMaintenance(ctx, root, owner, release, manual); err != nil {
		return err
	}
	force := manual
	if !manual {
		notice, err := autoupdate.LoadOwnerMaintenance(filepath.Join(root, "owner-maintenance.json"))
		if err != nil {
			return err
		}
		candidate, err := workerupdate.PreparedCandidateForRelease(release)
		if err != nil {
			return err
		}
		force = notice != nil && notice.CandidateID == candidate.ID && !time.Now().Before(notice.Deadline)
	}
	if err := owner.PrepareMaintenance(ctx, force); err != nil {
		if !errors.Is(err, hostdproto.ErrMaintenanceBusy) || manual {
			return err
		}
		if err := pendingOwnerMaintenance(root, release); err != nil {
			return err
		}
		// An elapsed deadline cannot override in-flight IO. Retry safely rather
		// than restart across an unfinished admitted mutation.
		notice, loadErr := autoupdate.LoadOwnerMaintenance(filepath.Join(root, "owner-maintenance.json"))
		if loadErr != nil {
			return loadErr
		}
		return &autoupdate.OwnerMaintenancePendingError{Notice: *notice}
	}
	return nil
}
