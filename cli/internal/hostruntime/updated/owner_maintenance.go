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

// Planning checks reachability without taking the admission lease. The actual
// transaction fences new work and drains exec before deciding whether remaining
// protected work prevents replacement.
func planOwnerMaintenance(ctx context.Context, root string, owner maintenanceOwner, release workerupdate.Release, force bool) error {
	if !release.SupervisorMaintenance || force {
		return nil
	}
	_, err := owner.Active(ctx)
	return err
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
	return &autoupdate.OwnerMaintenancePendingError{Notice: notice}
}

func authorizeOwnerMaintenance(ctx context.Context, root string, owner maintenanceOwner, release workerupdate.Release, force bool) error {
	if !release.SupervisorMaintenance {
		return nil
	}
	if err := planOwnerMaintenance(ctx, root, owner, release, force); err != nil {
		return err
	}
	if err := owner.PrepareMaintenance(ctx, force); err != nil {
		if !errors.Is(err, hostdproto.ErrMaintenanceBusy) || force {
			return err
		}
		return pendingOwnerMaintenance(root, release)
	}
	return nil
}
