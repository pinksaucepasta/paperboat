//go:build darwin || linux

package workerupdate

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

func TestPreparedSignedUpdatesUseNativeOwnerWithoutChangingApproval(t *testing.T) {
	for _, mode := range []string{"ordinary", "supervisor", "finalization fails"} {
		t.Run(mode, func(t *testing.T) {
			supervisor := mode == "supervisor"
			f := newFixture(t)
			finalizationFailure := errors.New("manual refresh is pending")
			if mode == "finalization fails" {
				f.manager.config.FinalizePrepared = func(context.Context, installsource.Source) error { return finalizationFailure }
			}
			f.candidate.SupervisorMaintenance = supervisor
			expected, err := PreparedCandidateForRelease(f.candidate)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			f.manager.config.ApplyPrepared = func(ctx context.Context, a PreparedActivation) (installsource.Source, error) {
				calls++
				identity, err := PreparedCandidateForRelease(a.Signed)
				if err != nil || identity != expected || a.Candidate != expected || a.Signed.SupervisorMaintenance != supervisor {
					t.Fatal("native installation changed the signed approval identity")
				}
				if err := a.Source.Verify(a.Executable); err != nil {
					t.Fatal(err)
				}
				if err := a.Prepare(ctx); err != nil {
					return installsource.Source{}, err
				}
				body, err := os.ReadFile(a.Executable)
				if err != nil {
					return installsource.Source{}, err
				}
				if err := os.WriteFile(f.paths.current, body, 0700); err != nil {
					return installsource.Source{}, err
				}
				f.hostd.active = hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(a.Source.Version), APIVersion: 1, Epoch: 42}
				if err := a.Validate(ctx); err != nil {
					return installsource.Source{}, err
				}
				return a.Source, nil
			}
			result, err := activatePrepared(context.Background(), f.manager, f.candidate)
			if mode == "finalization fails" {
				if !errors.Is(err, finalizationFailure) || result.Version != f.candidate.Version || f.manager.ActiveVersion() != f.candidate.Version || calls != 1 || f.hostd.stopActiveCalls != 0 {
					t.Fatalf("post-commit failure triggered old-owner rollback: %+v %v", result, err)
				}
				j, err := updateflow.Load(f.paths.journal)
				if err != nil || j.Stage != updateflow.StageAwaitingApproval || j.ApprovedCandidateID != expected.ID {
					t.Fatalf("finalization lost exact retry receipt: %+v %v", j, err)
				}
				return
			}
			if err != nil || !result.Updated || calls != 1 || f.hostd.stopActiveCalls != 0 {
				t.Fatalf("result=%+v err=%v native=%d stops=%d", result, err, calls, f.hostd.stopActiveCalls)
			}
			if _, err := os.Stat(f.paths.rollback); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("staging manager created a competing native rollback slot")
			}
			journal, err := updateflow.Load(f.paths.journal)
			if err != nil || journal.Stage != updateflow.StageIdle || journal.ActiveVersion != f.candidate.Version || journal.CandidateID != "" {
				t.Fatalf("staging receipt not retired: %+v %v", journal, err)
			}
		})
	}
}

func TestPreparedNativeFailureRetainsExactApprovalForOwnerRecovery(t *testing.T) {
	f := newFixture(t)
	failure := errors.New("native installation recovery pending")
	f.manager.config.ApplyPrepared = func(context.Context, PreparedActivation) (installsource.Source, error) {
		return installsource.Source{}, failure
	}
	_, err := activatePrepared(context.Background(), f.manager, f.candidate)
	if !errors.Is(err, failure) || f.manager.ActiveVersion() != f.active.Version || f.hostd.activations != 0 {
		t.Fatalf("native failure lost ownership: %v", err)
	}
	candidate, err := f.manager.PreparedCandidate()
	expected, expectedErr := PreparedCandidateForRelease(f.candidate)
	if err != nil || expectedErr != nil || candidate != expected {
		t.Fatalf("approved recovery candidate changed: %+v %v", candidate, err)
	}
	j, err := updateflow.Load(f.paths.journal)
	if err != nil || j.Stage != updateflow.StageAwaitingApproval || j.ApprovedCandidateID != expected.ID {
		t.Fatalf("native recovery approval lost: %+v %v", j, err)
	}
}

func TestNativeCommittedSourceSupersedesOnlyIdleStagingReceipt(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "pending"}[pending], func(t *testing.T) {
			f := newFixture(t)
			if pending {
				if _, err := f.manager.Prepare(context.Background(), f.candidate); err != nil {
					t.Fatal(err)
				}
			} else if err := f.manager.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			source, err := installsource.Inspect(f.paths.current, f.candidate.Version, installsource.Official)
			if err != nil {
				t.Fatal(err)
			}
			committed, err := InstalledBaseline(source, f.paths.current)
			if err != nil {
				t.Fatal(err)
			}
			f.manager.config.ApplyPrepared = func(context.Context, PreparedActivation) (installsource.Source, error) {
				t.Fatal("staging recovery started a native installation")
				return installsource.Source{}, nil
			}
			f.manager.setActive(committed)
			err = f.manager.Recover(context.Background())
			if pending {
				if !errors.Is(err, ErrBlocked) {
					t.Fatalf("pending approval was discarded: %v", err)
				}
			} else if err != nil {
				t.Fatalf("stale idle receipt blocked committed installation: %v", err)
			}
			j, err := updateflow.Load(f.paths.journal)
			if err != nil {
				t.Fatal(err)
			}
			if pending && j.ActiveVersion != f.active.Version || !pending && (j.ActiveVersion != source.Version || j.ActiveSource == nil || *j.ActiveSource != source) {
				t.Fatalf("wrong staging authority after recovery: %+v", j)
			}
		})
	}
}

func TestCompletedNativeRollbackRetiresItsGateTransaction(t *testing.T) {
	for _, mode := range []string{"drain", "health"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			failure := errors.New("candidate rejected")
			gate := &busyActivationGate{}
			if mode == "drain" {
				gate.drainErr = failure
			} else {
				f.health.err = failure
			}
			f.manager.config.Gate = gate
			previous, err := installsource.Inspect(f.paths.current, f.active.Version, installsource.Official)
			if err != nil {
				t.Fatal(err)
			}
			f.manager.config.ApplyPrepared = func(ctx context.Context, a PreparedActivation) (installsource.Source, error) {
				if err := a.Prepare(ctx); err != nil {
					return previous, err
				}
				baseline := f.hostd.active
				f.hostd.active = hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(a.Source.Version), APIVersion: 1, Epoch: 42}
				err := a.Validate(ctx)
				f.hostd.active = baseline
				return previous, err
			}
			_, err = activatePrepared(context.Background(), f.manager, f.candidate)
			if !errors.Is(err, failure) || gate.rollbacks != 1 {
				t.Fatalf("rollback did not finish the original gate: %v rollbacks=%d", err, gate.rollbacks)
			}
			j, err := updateflow.Load(f.paths.journal)
			if err != nil || j.Stage != updateflow.StageIdle || j.ActiveVersion != previous.Version || j.ApprovedCandidateID != "" || (j.CandidateVersion != "") != (mode == "health") {
				t.Fatalf("terminal gate would be retried as approved: %+v %v", j, err)
			}
			if err := previous.Verify(f.paths.current); err != nil {
				t.Fatal("staging manager changed the restored installation", err)
			}
		})
	}
}

// The native move consumes the staged payload. A helper that restarts after a failed finish
// (manual refresh, ledger commit) must complete it from the installed worker; it cannot ask
// for the staged file again. pbvm1 looped forever here with the update installed but pending.
func TestRetryAfterInterruptedFinishCompletesWithoutTheStagedPayload(t *testing.T) {
	f := newFixture(t)
	finalizeFails := true
	f.manager.config.FinalizePrepared = func(context.Context, installsource.Source) error {
		if finalizeFails {
			return errors.New("manual refresh is pending")
		}
		return nil
	}
	nativeCalls := 0
	f.manager.config.ApplyPrepared = func(ctx context.Context, a PreparedActivation) (installsource.Source, error) {
		nativeCalls++
		if err := a.Prepare(ctx); err != nil {
			return installsource.Source{}, err
		}
		body, err := os.ReadFile(a.Executable)
		if err != nil {
			return installsource.Source{}, err
		}
		if err := os.WriteFile(f.paths.current, body, 0700); err != nil {
			return installsource.Source{}, err
		}
		if err := os.Remove(a.Executable); err != nil {
			return installsource.Source{}, err
		}
		f.hostd.active = hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(a.Source.Version), APIVersion: 1, Epoch: 42}
		if err := a.Validate(ctx); err != nil {
			return installsource.Source{}, err
		}
		return a.Source, nil
	}
	previous := f.manager.active
	if _, err := activatePrepared(context.Background(), f.manager, f.candidate); err == nil {
		t.Fatal("the failing finish was not reported")
	}
	// Restart: the helper starts from the previous release again; the journal still holds the receipt.
	f.manager.setActive(previous)
	finalizeFails = false
	result, err := f.manager.Activate(context.Background(), f.candidate)
	if err != nil || !result.Updated || nativeCalls != 1 {
		t.Fatalf("retry did not finish the interrupted update: %+v %v native=%d", result, err, nativeCalls)
	}
	journal, err := updateflow.Load(f.paths.journal)
	if err != nil || journal.Stage != updateflow.StageIdle || journal.ActiveVersion != f.candidate.Version {
		t.Fatalf("receipt not retired by the retry: %+v %v", journal, err)
	}
}
