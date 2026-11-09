//go:build darwin || linux

package workerupdate

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

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
				// The failed finish is recorded, not left silent: counted, scheduled, same transaction.
				if !j.NativeCommitted || j.CommitAttempts != 1 || !j.NextCommitRetry.After(j.StageUpdatedAt.Add(-time.Second)) || j.LastFailure != updateflow.FailureNone {
					t.Fatalf("failed finish not recorded as commit-pending: %+v", j)
				}
				if state, err := f.manager.TransactionState(); err != nil || !state.CommitPending || state.CommitRetryAt.IsZero() {
					t.Fatalf("status does not show the pending finish: %+v %v", state, err)
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
	var completeErr error
	if f.candidate, completeErr = completeJournalRelease(f.candidate); completeErr != nil {
		t.Fatal(completeErr)
	}
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
	if j, err := updateflow.Load(f.paths.journal); err != nil || !j.NativeCommitted || j.CommitAttempts != 1 {
		t.Fatalf("manuals-only failure not recorded as commit-pending: %+v %v", j, err)
	}
	// hostd's commit check reads updater status, which asks for the prepared candidate: it must answer.
	if candidate, err := f.manager.PreparedCandidate(); err != nil || candidate.ID != "" {
		t.Fatalf("status cannot describe an installed candidate: %+v %v", candidate, err)
	}
	// Restart: the helper starts from the previous release again; the journal still holds the receipt.
	f.manager.setActive(previous)
	finalizeFails = false
	f.manager.config.RecoverPrepared = func(context.Context) (installsource.Source, error) {
		return f.manager.candidateSource(f.candidate), nil
	}
	// The helper first asks the manager for the approved release, as resolveUnixPreparedRelease does.
	if _, _, err := f.manager.PreparedRelease(context.Background()); err != nil {
		t.Fatalf("approved release unavailable once its payload is installed: %v", err)
	}
	result, err := f.manager.Activate(context.Background(), f.candidate)
	if err != nil || !result.Updated || nativeCalls != 1 {
		t.Fatalf("retry did not finish the interrupted update: %+v %v native=%d", result, err, nativeCalls)
	}
	journal, err := updateflow.Load(f.paths.journal)
	if err != nil || journal.Stage != updateflow.StageIdle || journal.ActiveVersion != f.candidate.Version || journal.NativeCommitted || journal.CommitAttempts != 0 {
		t.Fatalf("receipt not retired by the retry: %+v %v", journal, err)
	}
}

// A finish that never succeeds (hostd keeps refusing the commit) must not block the machine forever: after the
// attempt cap the journal ends idle on the installed version, the candidate quarantined, with commit_failed so
// status and doctor say it needs attention; every attempt before that keeps the approval and counts up.
func TestPersistentCommitFailureEndsQuarantinedAfterTheAttemptCap(t *testing.T) {
	f := newFixture(t)
	var completeErr error
	if f.candidate, completeErr = completeJournalRelease(f.candidate); completeErr != nil {
		t.Fatal(completeErr)
	}
	gate := &commitRefusingGate{}
	f.manager.config.Gate = gate
	f.manager.config.ApplyPrepared = func(ctx context.Context, a PreparedActivation) (installsource.Source, error) {
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
	f.manager.config.RecoverPrepared = func(context.Context) (installsource.Source, error) {
		return f.manager.candidateSource(f.candidate), nil
	}
	if _, err := activatePrepared(context.Background(), f.manager, f.candidate); err == nil {
		t.Fatal("the refused commit was not reported")
	}
	for attempt := uint32(1); attempt < maxCommitAttempts; attempt++ {
		j, err := updateflow.Load(f.paths.journal)
		if err != nil || j.Stage != updateflow.StageAwaitingApproval || j.ApprovedCandidateID == "" || !j.NativeCommitted || j.CommitAttempts != attempt {
			t.Fatalf("attempt %d: approval/commit-pending not kept: %+v %v", attempt, j, err)
		}
		f.manager.setActive(previous) // the helper restarts on the previous release
		if _, err := f.manager.Activate(context.Background(), f.candidate); err == nil {
			t.Fatalf("attempt %d: the refused commit was not reported", attempt+1)
		}
	}
	j, err := updateflow.Load(f.paths.journal)
	if err != nil || j.Stage != updateflow.StageIdle || j.LastFailure != updateflow.FailureCommit || j.NativeCommitted || j.ApprovedCandidateID != "" || j.ActiveVersion != f.candidate.Version || j.CandidateVersion != f.candidate.Version {
		t.Fatalf("persistent commit failure did not reach the terminal state: %+v %v", j, err)
	}
	state, err := f.manager.TransactionState()
	if err != nil || state.CommitPending || !state.Quarantined || state.Failure != updateflow.FailureCommit {
		t.Fatalf("terminal state not reported: %+v %v", state, err)
	}
	if gate.commits != int(maxCommitAttempts) {
		t.Fatalf("hostd was asked to commit %d times, want %d", gate.commits, maxCommitAttempts)
	}
}

// luf26a: a network loss during the participant step failed an update after the native owner had
// put the candidate in the worker slot and consumed the staged payload, but before its commit. The
// next helper starts saw the candidate's bytes, took them for a committed install and asked hostd to
// commit; hostd refused (maintenance busy) because the source was never committed, every 7 s, for
// hours. Native recovery now decides first: a restored previous version ends the attempt.
func TestInstalledCandidateWithoutNativeCommitIsRolledBackNotCommitted(t *testing.T) {
	f := newFixture(t)
	var completeErr error
	if f.candidate, completeErr = completeJournalRelease(f.candidate); completeErr != nil {
		t.Fatal(completeErr)
	}
	gate := &commitRefusingGate{}
	f.manager.config.Gate = gate
	f.manager.config.ApplyPrepared = func(ctx context.Context, a PreparedActivation) (installsource.Source, error) {
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
		return installsource.Source{}, errors.New("participant readiness: lookup failed; service lifecycle outcome is uncertain")
	}
	previous := f.manager.active
	if _, err := activatePrepared(context.Background(), f.manager, f.candidate); err == nil {
		t.Fatal("the interrupted activation was not reported")
	}
	restored := installsource.Source{Version: previous.Version, Platform: previous.Platform, Architecture: previous.Architecture, SHA256: previous.SHA256, Length: previous.Length, Distribution: installsource.Official}
	recoveries := 0
	f.manager.config.RecoverPrepared = func(context.Context) (installsource.Source, error) {
		recoveries++
		return restored, nil
	}
	// Restart: the helper runs the previous release and the journal still holds the approval.
	f.manager.setActive(previous)
	result, err := f.manager.Activate(context.Background(), f.candidate)
	if !errors.Is(err, ErrNativeRestored) || result.Updated || recoveries != 1 {
		t.Fatalf("restored installation not reported as a rollback: %+v %v recoveries=%d", result, err, recoveries)
	}
	if gate.commits != 0 || gate.rollbacks != 1 {
		t.Fatalf("hostd gate commits=%d rollbacks=%d, want the candidate's gate released, never committed", gate.commits, gate.rollbacks)
	}
	journal, err := updateflow.Load(f.paths.journal)
	if err != nil || journal.Stage != updateflow.StageIdle || journal.ActiveVersion != previous.Version || journal.LastFailure != updateflow.FailureHealth {
		t.Fatalf("attempt not ended on the previous version: %+v %v", journal, err)
	}
	if state, err := f.manager.TransactionState(); err != nil || !state.Quarantined {
		t.Fatalf("the failed candidate is not quarantined, so it would be retried: %+v %v", state, err)
	}
}

// commitRefusingGate is hostd while the native source is uncommitted: it refuses every commit.
type commitRefusingGate struct {
	fakeActivationGate
	commits, rollbacks int
}

func (g *commitRefusingGate) Commit(context.Context, GateRequest) error {
	g.commits++
	return hostdproto.ErrMaintenanceBusy
}

func (g *commitRefusingGate) Rollback(context.Context, GateRequest) error {
	g.rollbacks++
	return nil
}

// A rolled-back or never-started installation consumes the staged payload. The approval for it can
// then never be used: it is reset to idle with a failure so the updater stops retrying it on every
// start (a restart loop that also blocked checking for a replacement).
func TestApprovedCandidateWhoseStagedPayloadIsGoneIsAbandoned(t *testing.T) {
	f := newFixture(t)
	candidate, err := f.manager.Prepare(context.Background(), f.candidate)
	if err != nil || candidate.ID == "" {
		t.Fatalf("prepare: %+v %v", candidate, err)
	}
	if abandoned, err := f.manager.AbandonUnavailableCandidate(context.Background()); abandoned || err != nil {
		t.Fatalf("a candidate that is not approved was abandoned: %v %v", abandoned, err)
	}
	if err := f.manager.Approve(context.Background(), candidate.ID); err != nil {
		t.Fatal(err)
	}
	if abandoned, err := f.manager.AbandonUnavailableCandidate(context.Background()); abandoned || err != nil {
		t.Fatalf("an approved candidate that is still staged was abandoned: %v %v", abandoned, err)
	}
	journal, err := updateflow.Load(f.paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	// The worker slot still holds the previous release, not the candidate's bytes.
	if err := os.WriteFile(f.paths.current, []byte("previous release"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(journal.StagedPath); err != nil {
		t.Fatal(err)
	}
	abandoned, err := f.manager.AbandonUnavailableCandidate(context.Background())
	if !abandoned || err != nil {
		t.Fatalf("an approved candidate with no payload was kept: %v %v", abandoned, err)
	}
	journal, err = updateflow.Load(f.paths.journal)
	if err != nil || journal.Stage != updateflow.StageIdle || journal.LastFailure != updateflow.FailureCandidate || journal.CandidateID != "" {
		t.Fatalf("journal not reset to idle with the failure: %+v %v", journal, err)
	}
	if _, err := f.manager.Prepare(context.Background(), f.candidate); err != nil {
		t.Fatalf("a replacement candidate cannot be prepared after the reset: %v", err)
	}
}

// A newer release published after approval makes the activation helper refuse the approved payload
// (ErrCandidateChanged) while the payload is still staged. The attempt must end fully: before this,
// the handoff stayed Started, every restart repeated the refusal, downloads, automatic updates and
// uninstall were blocked forever.
func TestApprovedCandidateSupersededWhileStagedIsAbandonedWithItsPayload(t *testing.T) {
	f := newFixture(t)
	candidate, err := f.manager.Prepare(context.Background(), f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	if abandoned, err := f.manager.AbandonSupersededCandidate(context.Background()); abandoned || err != nil {
		t.Fatalf("a candidate that is not approved was abandoned: %v %v", abandoned, err)
	}
	if err := f.manager.Approve(context.Background(), candidate.ID); err != nil {
		t.Fatal(err)
	}
	journal, err := updateflow.Load(f.paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(journal.StagedPath); err != nil {
		t.Fatalf("approved payload is not staged: %v", err)
	}
	// The unavailable-payload variant must keep refusing a payload that is still there.
	if abandoned, err := f.manager.AbandonUnavailableCandidate(context.Background()); abandoned || err != nil {
		t.Fatalf("a staged payload was treated as unavailable: %v %v", abandoned, err)
	}
	// A payload a native installation already moved is owned by that installation: recovery must run.
	saved, err := os.ReadFile(journal.StagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(journal.StagedPath); err != nil {
		t.Fatal(err)
	}
	if abandoned, err := f.manager.AbandonSupersededCandidate(context.Background()); abandoned || err != nil {
		t.Fatalf("a candidate whose payload native activation took was abandoned: %v %v", abandoned, err)
	}
	if err := os.WriteFile(journal.StagedPath, saved, 0700); err != nil {
		t.Fatal(err)
	}
	if abandoned, err := f.manager.AbandonSupersededCandidate(context.Background()); !abandoned || err != nil {
		t.Fatalf("a superseded approved candidate was kept: %v %v", abandoned, err)
	}
	if _, err := os.Lstat(journal.StagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged payload survived the abandon: %v", err)
	}
	journal, err = updateflow.Load(f.paths.journal)
	if err != nil || journal.Stage != updateflow.StageIdle || journal.LastFailure != updateflow.FailureCandidate || journal.CandidateID != "" || journal.ApprovedCandidateID != "" {
		t.Fatalf("journal not reset to idle with the failure: %+v %v", journal, err)
	}
	if _, err := f.manager.Prepare(context.Background(), f.candidate); err != nil {
		t.Fatalf("the new candidate cannot be downloaded after the reset: %v", err)
	}
}

// A candidate the native owner already committed whose finish current signed policy refuses (it was revoked, or the
// version it replaced was) ends visibly instead of retrying forever: idle on the installed version, quarantined,
// release_revoked, and a newer authorized release can be downloaded to supersede it. An uncommitted or still staged
// candidate is left alone: the native owner (or the superseded path) owns it.
func TestRevokedCommittedCandidateEndsTerminalAndIsSupersedable(t *testing.T) {
	f := newFixture(t)
	var completeErr error
	if f.candidate, completeErr = completeJournalRelease(f.candidate); completeErr != nil {
		t.Fatal(completeErr)
	}
	f.manager.config.FinalizePrepared = func(context.Context, installsource.Source) error { return errors.New("manual refresh is pending") }
	f.manager.config.ApplyPrepared = func(ctx context.Context, a PreparedActivation) (installsource.Source, error) {
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
	f.manager.setActive(previous) // the helper restarts on the previous release
	committed := f.manager.candidateSource(f.candidate)
	// A different committed source is not this candidate.
	other := committed
	other.Version = "2026.08.18.9"
	if ended, err := f.manager.EndRevokedCommittedCandidate(context.Background(), other); ended || err != nil {
		t.Fatalf("a source that is not the candidate ended the attempt: %v %v", ended, err)
	}
	if ended, err := f.manager.EndRevokedCommittedCandidate(context.Background(), committed); !ended || err != nil {
		t.Fatalf("the revoked committed candidate was kept: %v %v", ended, err)
	}
	j, err := updateflow.Load(f.paths.journal)
	if err != nil || j.Stage != updateflow.StageIdle || j.LastFailure != updateflow.FailureRevoked || j.NativeCommitted || j.ApprovedCandidateID != "" || j.ActiveVersion != f.candidate.Version || j.CandidateVersion != f.candidate.Version {
		t.Fatalf("not terminal on the installed version: %+v %v", j, err)
	}
	if state, err := f.manager.TransactionState(); err != nil || !state.Quarantined || state.Failure != updateflow.FailureRevoked || state.CommitPending {
		t.Fatalf("terminal state not reported: %+v %v", state, err)
	}
	// Supersedable: a newer release downloads and is offered for approval.
	body, err := os.ReadFile(f.paths.current)
	if err != nil {
		t.Fatal(err)
	}
	if candidate, err := f.manager.Prepare(context.Background(), release("2026.08.18.3", body)); err != nil || candidate.ID == "" {
		t.Fatalf("a newer release cannot supersede the ended attempt: %+v %v", candidate, err)
	}
}

// An interrupted native replacement that recovery undid leaves the previous installation standing. The attempt ends
// on it (staged payload gone, idle journal, no helper repeats it) whether or not the payload was already consumed;
// when signed policy refuses the restored version too MarkActiveRefused turns the journal into release_revoked, the
// terminal state status and doctor report as recovery_required. A source that is not the active release, or a
// journal that is not an approved candidate, ends nothing.
func TestRecoveredCandidateEndsOnTheRestoredVersion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		consumed    bool
		refused     bool
		wantFailure updateflow.Failure
	}{
		{"payload still staged, previous allowed", false, false, updateflow.FailureCandidate},
		{"payload consumed, previous allowed", true, false, updateflow.FailureCandidate},
		{"payload consumed, previous refused", true, true, updateflow.FailureRevoked},
		{"payload still staged, previous refused", false, true, updateflow.FailureRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			// The fixture's candidate shares the active executable's bytes; a recovered attempt has different ones.
			body := append(append([]byte(nil), f.fetcher.body...), 0)
			f.fetcher.body, f.candidate = body, release(f.candidate.Version, body)
			candidate, err := f.manager.Prepare(context.Background(), f.candidate)
			if err != nil {
				t.Fatal(err)
			}
			restored := f.manager.candidateSource(f.manager.active)
			// Not yet approved: nothing to end.
			if ended, err := f.manager.EndRecoveredCandidate(context.Background(), restored); ended || err != nil {
				t.Fatalf("an unapproved candidate was ended: %v %v", ended, err)
			}
			if err := f.manager.Approve(context.Background(), candidate.ID); err != nil {
				t.Fatal(err)
			}
			staged, err := updateflow.Load(f.paths.journal)
			if err != nil {
				t.Fatal(err)
			}
			if tc.consumed {
				if err := os.Remove(staged.StagedPath); err != nil {
					t.Fatal(err)
				}
			}
			other := restored
			other.Version = "2026.08.18.9"
			if ended, err := f.manager.EndRecoveredCandidate(context.Background(), other); ended || err != nil {
				t.Fatalf("a source that is not the active release ended the attempt: %v %v", ended, err)
			}
			if ended, err := f.manager.EndRecoveredCandidate(context.Background(), restored); !ended || err != nil {
				t.Fatalf("the recovered attempt was kept: %v %v", ended, err)
			}
			if _, err := os.Lstat(staged.StagedPath); !os.IsNotExist(err) {
				t.Fatalf("staged payload survived: %v", err)
			}
			wrote, err := f.manager.MarkActiveRefused(context.Background(), func(Release) bool { return tc.refused })
			if err != nil || wrote != tc.refused {
				t.Fatalf("MarkActiveRefused wrote=%v err=%v, want %v", wrote, err, tc.refused)
			}
			if tc.refused {
				if wrote, err = f.manager.MarkActiveRefused(context.Background(), func(Release) bool { return true }); wrote || err != nil {
					t.Fatalf("the terminal state was rewritten: %v %v", wrote, err)
				}
			}
			j, err := updateflow.Load(f.paths.journal)
			if err != nil || j.Stage != updateflow.StageIdle || j.LastFailure != tc.wantFailure || j.ApprovedCandidateID != "" || j.ActiveVersion != f.manager.active.Version {
				t.Fatalf("not idle on the restored version: %+v %v", j, err)
			}
			if state, err := f.manager.TransactionState(); err != nil || state.Failure != tc.wantFailure {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			// Supersedable: a newer release downloads again.
			if _, err := f.manager.Prepare(context.Background(), release("2026.08.18.3", f.fetcher.body)); err != nil {
				t.Fatalf("a newer release cannot supersede the ended attempt: %v", err)
			}
		})
	}
}
