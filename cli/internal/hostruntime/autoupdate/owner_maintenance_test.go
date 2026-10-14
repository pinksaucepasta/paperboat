package autoupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releasepolicy"
)

func TestPlanOwnerMaintenancePersistsAndExactRetryKeepsDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner-maintenance.json")
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.FixedZone("test", 5*60*60+30*60))

	missing, err := LoadOwnerMaintenance(path)
	if err != nil || missing != nil {
		t.Fatalf("load missing notice = %v, %v; want nil, nil", missing, err)
	}

	first, err := PlanOwnerMaintenance(path, maintenanceTestCandidate('a'), "2026.10.08.1", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Schema != ownerMaintenanceSchema || !first.AnnouncedAt.Equal(now) || !first.Deadline.Equal(now.Add(2*time.Second)) {
		t.Fatalf("first notice = %+v", first)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("notice permissions = %o, want 600", info.Mode().Perm())
	}

	// A process restart reloads the same notice. Retrying at the deadline and
	// after it cannot move the original deadline forward.
	loaded, err := LoadOwnerMaintenance(path)
	if err != nil || loaded == nil || *loaded != first {
		t.Fatalf("loaded notice = %+v, %v; want %+v", loaded, err, first)
	}
	for _, retryAt := range []time.Time{first.Deadline, first.Deadline.Add(time.Hour)} {
		retry, err := PlanOwnerMaintenance(path, first.CandidateID, first.Version, releasepolicy.MaxRoutineDeferralSec, retryAt)
		if err != nil {
			t.Fatal(err)
		}
		if retry != first {
			t.Fatalf("retry at %s changed notice to %+v; want %+v", retryAt, retry, first)
		}
	}

	pendingErr := OwnerMaintenancePendingError{Notice: first}
	if strings.Contains(pendingErr.Error(), first.CandidateID) || strings.Contains(pendingErr.Error(), first.Version) {
		t.Fatalf("pending error leaks release identity: %q", pendingErr.Error())
	}
}

func TestPlanOwnerMaintenanceUsesEarliestDeadlineForNewCandidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner-maintenance.json")
	start := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	first, err := PlanOwnerMaintenance(path, maintenanceTestCandidate('a'), "2026.10.08.1", 3600, start)
	if err != nil {
		t.Fatal(err)
	}

	secondAt := start.Add(10 * time.Minute)
	second, err := PlanOwnerMaintenance(path, maintenanceTestCandidate('b'), "2026.10.08.2", 48*60*60, secondAt)
	if err != nil {
		t.Fatal(err)
	}
	if !second.AnnouncedAt.Equal(secondAt) || !second.Deadline.Equal(first.Deadline) {
		t.Fatalf("new candidate postponed earlier pending deadline: first=%+v second=%+v", first, second)
	}

	// A shorter new grace is allowed to bring maintenance forward.
	thirdAt := secondAt.Add(5 * time.Minute)
	third, err := PlanOwnerMaintenance(path, maintenanceTestCandidate('c'), "2026.10.08.3", 60, thirdAt)
	if err != nil {
		t.Fatal(err)
	}
	if !third.Deadline.Equal(thirdAt.Add(time.Minute)) || !third.Deadline.Before(second.Deadline) {
		t.Fatalf("new candidate did not preserve the earliest deadline: %+v", third)
	}

	// Later announcements retain that earlier deadline even with maximum grace.
	fourth, err := PlanOwnerMaintenance(path, maintenanceTestCandidate('d'), "2026.10.08.4", releasepolicy.MaxRoutineDeferralSec, thirdAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !fourth.Deadline.Equal(third.Deadline) {
		t.Fatalf("later candidate extended pending deadline: previous=%s new=%s", third.Deadline, fourth.Deadline)
	}
}

func TestPlanOwnerMaintenanceBoundsGraceAndDeadlineBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner-maintenance.json")
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	if _, err := PlanOwnerMaintenance(path, maintenanceTestCandidate('a'), "2026.10.08.1", releasepolicy.MaxRoutineDeferralSec+1, now); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("oversized grace error = %v, want ErrInvalidConfig", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized grace mutated state: stat error = %v", err)
	}

	immediate, err := PlanOwnerMaintenance(path, maintenanceTestCandidate('a'), "2026.10.08.1", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if !immediate.Deadline.Equal(immediate.AnnouncedAt) {
		t.Fatalf("zero-grace deadline = %s, announcement = %s", immediate.Deadline, immediate.AnnouncedAt)
	}

	maxPath := filepath.Join(t.TempDir(), "maximum-owner-maintenance.json")
	maximum, err := PlanOwnerMaintenance(maxPath, maintenanceTestCandidate('b'), "2026.10.08.2", releasepolicy.MaxRoutineDeferralSec, now)
	if err != nil {
		t.Fatal(err)
	}
	if !maximum.Deadline.Equal(maximum.AnnouncedAt.Add(time.Duration(releasepolicy.MaxRoutineDeferralSec) * time.Second)) {
		t.Fatalf("maximum grace deadline = %s", maximum.Deadline)
	}
}

func TestLoadOwnerMaintenanceRejectsMalformedStateWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner-maintenance.json")
	malformed := []byte(`{"schema":"paperboat.owner-maintenance/v1","candidate_id":"bad","version":"2026.10.08.1","announced_at":"2026-10-08T12:00:00Z","deadline":"2026-10-08T12:00:01Z","unexpected":true}` + "\n")
	if err := os.WriteFile(path, malformed, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadOwnerMaintenance(path); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("load malformed state error = %v, want ErrInvalidConfig", err)
	}
	if _, err := PlanOwnerMaintenance(path, maintenanceTestCandidate('a'), "2026.10.08.1", 10, time.Now()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("plan over malformed state error = %v, want ErrInvalidConfig", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(malformed) {
		t.Fatalf("malformed state changed: data=%q err=%v", after, err)
	}
}

func TestPlanOwnerMaintenanceInvalidInputDoesNotMutateExistingNotice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner-maintenance.json")
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	want, err := PlanOwnerMaintenance(path, maintenanceTestCandidate('a'), "2026.10.08.1", 60, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, input := range []struct {
		candidate string
		version   string
		grace     uint32
	}{
		{maintenanceTestCandidate('A'), "2026.10.08.1", 60},
		{maintenanceTestCandidate('b'), "2026.02.30.1", 60},
		{maintenanceTestCandidate('b'), "2026.10.08.2", releasepolicy.MaxRoutineDeferralSec + 1},
	} {
		if _, err := PlanOwnerMaintenance(path, input.candidate, input.version, input.grace, now.Add(time.Minute)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("invalid plan error = %v, want ErrInvalidConfig", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("invalid plan mutated notice %+v: data=%q err=%v", want, after, err)
		}
	}

	if _, err := PlanOwnerMaintenance(path, want.CandidateID, "2026.10.09.1", 60, now.Add(time.Minute)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("candidate/version mismatch error = %v, want ErrInvalidConfig", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("candidate/version mismatch mutated notice: data=%q err=%v", after, err)
	}
}

func TestClearOwnerMaintenanceIsIdempotentAndRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owner-maintenance.json")
	if err := ClearOwnerMaintenance(path); err != nil {
		t.Fatalf("clear missing notice: %v", err)
	}

	if _, err := PlanOwnerMaintenance(path, maintenanceTestCandidate('a'), "2026.10.08.1", 60, time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := ClearOwnerMaintenance(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleared notice still exists: %v", err)
	}
	if err := ClearOwnerMaintenance(path); err != nil {
		t.Fatalf("second clear: %v", err)
	}

	target := filepath.Join(dir, "target.json")
	targetData := []byte("target remains untouched\n")
	if err := os.WriteFile(target, targetData, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, symlink); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	if err := ClearOwnerMaintenance(symlink); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("clear symlink error = %v, want ErrInvalidConfig", err)
	}
	if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was removed or changed: info=%v err=%v", info, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(targetData) {
		t.Fatalf("symlink target changed: data=%q err=%v", got, err)
	}
}

func maintenanceTestCandidate(character byte) string {
	return strings.Repeat(string(character), 64)
}
