//go:build darwin || linux

package workerupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

func TestTRK35UpdateJournalClockBoundaryIsInclusive(t *testing.T) {
	fixture := newFixture(t)
	const timestamp = "2026-09-01T12:00:00Z"
	now, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		t.Fatal(err)
	}
	fixture.manager.config.Now = func() time.Time { return now }
	base := fixture.manager.newJournal()
	if !base.StageUpdatedAt.Equal(now) {
		t.Fatalf("new journal timestamp=%s want=%s", base.StageUpdatedAt, now)
	}
	if next, err := base.Transition(updateflow.StageChecking, now); err != nil || !next.StageUpdatedAt.Equal(now) {
		t.Fatalf("equal timestamp transition=%+v err=%v", next, err)
	}
	if _, err := base.Transition(updateflow.StageChecking, now.Add(-time.Nanosecond)); !errors.Is(err, updateflow.ErrInvalidTransition) {
		t.Fatalf("clock rollback transition error=%v", err)
	}
}

func TestTRK35CorruptJournalBlocksRecoveryWithoutTouchingRuntime(t *testing.T) {
	fixture := newFixture(t)
	currentBefore, err := os.ReadFile(fixture.paths.current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.paths.staged, []byte("candidate remains quarantined"), 0o700); err != nil {
		t.Fatal(err)
	}
	const privateMarker = "private-corrupt-journal-value"
	corrupt := []byte(`{"schema":17,"boot_id":"` + privateMarker + `"}`)
	if err := os.WriteFile(fixture.paths.journal, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.manager.TransactionState()
	var typeErr *json.UnmarshalTypeError
	if !errors.Is(err, updateflow.ErrInvalidJournal) || !errors.As(err, &typeErr) {
		t.Fatalf("TransactionState error=%v, want invalid journal", err)
	}
	if strings.Contains(err.Error(), fixture.paths.journal) || strings.Contains(err.Error(), privateMarker) {
		t.Fatalf("TransactionState exposed corrupt journal details: %q", err)
	}
	recoveryErr := fixture.manager.Recover(context.Background())
	typeErr = nil
	if !errors.Is(recoveryErr, ErrBlocked) || !errors.Is(recoveryErr, updateflow.ErrInvalidJournal) || !errors.As(recoveryErr, &typeErr) {
		t.Fatalf("corrupt journal recovery error=%v, want ErrBlocked and original parse cause", recoveryErr)
	}
	if strings.Contains(recoveryErr.Error(), fixture.paths.journal) || strings.Contains(recoveryErr.Error(), privateMarker) {
		t.Fatalf("recovery exposed corrupt journal details: %q", recoveryErr)
	}
	currentAfter, err := os.ReadFile(fixture.paths.current)
	if err != nil || !bytes.Equal(currentAfter, currentBefore) {
		t.Fatalf("corrupt journal changed active runtime err=%v equal=%v", err, bytes.Equal(currentAfter, currentBefore))
	}
	if _, err := os.Stat(fixture.paths.staged); err != nil {
		t.Fatalf("corrupt journal removed staged evidence: %v", err)
	}
	if fixture.manager.ActiveVersion() != fixture.active.Version {
		t.Fatalf("corrupt journal changed active version=%q", fixture.manager.ActiveVersion())
	}
}

func TestTRK35QuarantineSurvivesRestartAndAllowsOnlyNewerRelease(t *testing.T) {
	fixture := newFixture(t)
	now := time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)
	fixture.manager.config.Now = func() time.Time { return now }
	gate := &scriptedGate{activeErr: errors.New("candidate rejected")}
	fixture.manager.config.Gate = gate
	if _, err := activatePrepared(context.Background(), fixture.manager, fixture.candidate); !errors.Is(err, ErrActivationGate) {
		t.Fatalf("candidate failure error=%v", err)
	}
	quarantined, err := fixture.manager.TransactionState()
	if err != nil || !quarantined.Quarantined || quarantined.Stage != updateflow.StageIdle || quarantined.CandidateVersion != fixture.candidate.Version {
		t.Fatalf("quarantine state=%+v err=%v", quarantined, err)
	}

	restarted, err := New(fixture.manager.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := activatePrepared(context.Background(), restarted, fixture.candidate); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("exact quarantined retry error=%v", err)
	}
	restarted.config.Gate = &scriptedGate{}
	newer := release("2026.08.18.3", fixture.fetcher.body)
	result, err := activatePrepared(context.Background(), restarted, newer)
	if err != nil || !result.Updated || restarted.ActiveVersion() != newer.Version {
		t.Fatalf("newer release result=%+v err=%v active=%q", result, err, restarted.ActiveVersion())
	}
}
