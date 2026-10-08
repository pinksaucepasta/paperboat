//go:build darwin || linux || windows

package updated

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

type maintenanceTestOwner struct {
	protected uint64
	forces    []bool
	busy      bool
}

func (o *maintenanceTestOwner) Active(context.Context) (hostdproto.Status, error) {
	return hostdproto.Status{ProtectedWorkloads: o.protected}, nil
}
func (o *maintenanceTestOwner) PrepareMaintenance(_ context.Context, force bool) error {
	o.forces = append(o.forces, force)
	if o.busy {
		return hostdproto.ErrMaintenanceBusy
	}
	return nil
}
func maintenanceTestRelease() workerupdate.Release {
	return workerupdate.Release{Version: "2026.10.08.20", SHA256: strings.Repeat("a", 64), Length: 1, Platform: runtime.GOOS, Architecture: runtime.GOARCH, HostdAPIMin: 1, HostdAPIMax: 1, RuntimeAPIMin: 1, RuntimeAPIMax: 1, SupervisorMaintenance: true, OwnerMaintenanceGraceSeconds: 60}
}
func TestOwnerMaintenanceAdmissionAndDeadline(t *testing.T) {
	root := t.TempDir()
	release := maintenanceTestRelease()
	owner := &maintenanceTestOwner{protected: 1}
	err := authorizeOwnerMaintenance(context.Background(), root, owner, release, false)
	var pending *autoupdate.OwnerMaintenancePendingError
	if !errors.As(err, &pending) || len(owner.forces) != 0 {
		t.Fatalf("lease before notice deadline: %v, %v", err, owner.forces)
	}
	candidate, err := workerupdate.PreparedCandidateForRelease(release)
	if err != nil {
		t.Fatal(err)
	}
	otherRoot := t.TempDir()
	_, err = autoupdate.PlanOwnerMaintenance(filepath.Join(otherRoot, "owner-maintenance.json"), candidate.ID, release.Version, 60, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = authorizeOwnerMaintenance(context.Background(), otherRoot, owner, release, false); err != nil || len(owner.forces) != 1 || !owner.forces[0] {
		t.Fatalf("expired announcement: %v, %v", err, owner.forces)
	}
	owner.busy = true
	err = authorizeOwnerMaintenance(context.Background(), otherRoot, owner, release, false)
	if !errors.As(err, &pending) {
		t.Fatalf("inflight IO bypassed: %v", err)
	}
}
func TestOwnerMaintenanceManualAndOrdinary(t *testing.T) {
	owner := &maintenanceTestOwner{protected: 1}
	root := t.TempDir()
	release := maintenanceTestRelease()
	if err := authorizeOwnerMaintenance(context.Background(), root, owner, release, true); err != nil || len(owner.forces) != 1 || !owner.forces[0] {
		t.Fatalf("explicit approval: %v, %v", err, owner.forces)
	}
	release.SupervisorMaintenance = false
	if err := authorizeOwnerMaintenance(context.Background(), root, owner, release, false); err != nil || len(owner.forces) != 1 {
		t.Fatalf("ordinary replacement takes owner lease: %v, %v", err, owner.forces)
	}
	notice, err := autoupdate.LoadOwnerMaintenance(filepath.Join(root, "owner-maintenance.json"))
	if err != nil || notice != nil {
		t.Fatalf("unexpected announcement: %v, %v", notice, err)
	}
}
func TestOwnerMaintenanceAdmissionRaceAnnounces(t *testing.T) {
	owner := &maintenanceTestOwner{busy: true}
	root := t.TempDir()
	release := maintenanceTestRelease()
	err := authorizeOwnerMaintenance(context.Background(), root, owner, release, false)
	var pending *autoupdate.OwnerMaintenancePendingError
	if !errors.As(err, &pending) || len(owner.forces) != 1 || owner.forces[0] {
		t.Fatalf("race did not announce safely: %v, %v", err, owner.forces)
	}
}
