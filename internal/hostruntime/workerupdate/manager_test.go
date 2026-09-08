//go:build darwin || linux

package workerupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

func TestWorkerUpdateCutsOverWithoutRestartingHostd(t *testing.T) {
	fixture := newFixture(t)
	result, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.Version != fixture.candidate.Version {
		t.Fatalf("result=%+v", result)
	}
	if fixture.hostd.activations != 1 || fixture.hostd.active.WorkerID != workerID(fixture.candidate.Version) {
		t.Fatalf("hostd=%+v", fixture.hostd)
	}
	if fixture.starter.starts != 1 || fixture.starter.requests[0].Executable != fixture.paths.staged || fixture.starter.requests[0].UID <= 0 || !fixture.starter.requests[0].MutationsDisabled {
		t.Fatalf("start requests=%+v", fixture.starter.requests)
	}
	if _, err := os.Stat(fixture.paths.staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged runtime remains after commit: %v", err)
	}
	if !regularMatches(fixture.paths.current, fixture.candidate.Length, fixture.candidate.SHA256) || !regularMatches(fixture.paths.rollback, fixture.active.Length, fixture.active.SHA256) {
		t.Fatal("active/rollback runtime retention is incorrect")
	}
	if info, err := os.Stat(fixture.paths.current); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("active runtime is not executable: info=%v err=%v", info, err)
	}
	journal, err := updateflow.Load(fixture.paths.journal)
	if err != nil || journal.Stage != updateflow.StageIdle || journal.ActiveVersion != fixture.candidate.Version {
		t.Fatalf("journal=%+v err=%v", journal, err)
	}
	recovered, err := ActiveReleaseFromJournal(fixture.paths.journal, fixture.candidate.Version)
	if err != nil || !sameReleaseTargets(recovered, fixture.candidate) {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
}

func TestActiveVersionRemainsReadableDuringHealthHold(t *testing.T) {
	fixture := newFixture(t)
	health := &blockingHealth{entered: make(chan struct{}, 1), release: make(chan struct{})}
	fixture.manager.config.Health = health
	updateDone := make(chan error, 1)
	released := false
	defer func() {
		if !released {
			close(health.release)
		}
	}()
	go func() {
		_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
		updateDone <- err
	}()
	select {
	case <-health.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("update did not enter its health hold")
	}
	version := make(chan string, 1)
	go func() { version <- fixture.manager.ActiveVersion() }()
	select {
	case got := <-version:
		if got != fixture.active.Version {
			t.Fatalf("active version during hold = %q, want %q", got, fixture.active.Version)
		}
	case <-time.After(time.Second):
		t.Fatal("active version blocked behind the update transaction")
	}
	close(health.release)
	released = true
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerUpdateSeedsSignedActiveStateBeforeFirstUpdate(t *testing.T) {
	fixture := newFixture(t)
	if err := fixture.manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	journal, err := updateflow.Load(fixture.paths.journal)
	if err != nil || journal.Stage != updateflow.StageIdle || journal.ActiveVersion != fixture.active.Version || journal.ActiveDigest != fixture.active.SHA256 {
		t.Fatalf("journal=%+v err=%v", journal, err)
	}
	recovered, err := RecoveryReleaseFromJournal(fixture.paths.journal, fixture.active.Version)
	if err != nil || !sameReleaseTargets(recovered, fixture.active) {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
}

func TestNewerNativePackageSupersedesBlockedPreCutoverWorkerTransaction(t *testing.T) {
	fixture := newFixture(t)
	now := time.Now().UTC()
	journal := withRelease(withActiveRelease(updateflow.Journal{
		Schema: updateflow.SchemaV1, TransactionID: "txn-blocked-package-upgrade", Stage: updateflow.StageBlocked,
		BootID: "hostd", StageUpdatedAt: now, LastFailure: updateflow.FailureDrain,
	}, fixture.active), fixture.candidate, fixture.paths.staged)
	if err := os.WriteFile(fixture.paths.staged, fixture.fetcher.body, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := updateflow.Write(fixture.paths.journal, journal, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	packaged := release("2026.08.18.3", fixture.fetcher.body)
	fixture.manager.active = packaged
	fixture.manager.activeVersion.Store(packaged.Version)
	if err := fixture.manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := updateflow.Load(fixture.paths.journal)
	if err != nil || recovered.Stage != updateflow.StageIdle || recovered.ActiveVersion != packaged.Version || recovered.ActiveDigest != packaged.SHA256 {
		t.Fatalf("journal=%+v err=%v", recovered, err)
	}
	if _, err := os.Stat(fixture.paths.staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("superseded staged candidate remains: %v", err)
	}
}

func TestNewerNativePackageSupersedesIdleOlderJournal(t *testing.T) {
	fixture := newFixture(t)
	journal := withActiveRelease(updateflow.Journal{
		Schema: updateflow.SchemaV1, TransactionID: "txn-idle-package-upgrade", Stage: updateflow.StageIdle,
		BootID: "hostd", StageUpdatedAt: time.Now().UTC(),
	}, fixture.active)
	if err := updateflow.Write(fixture.paths.journal, journal, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	packaged := release("2026.08.18.3", fixture.fetcher.body)
	fixture.manager.active = packaged
	fixture.manager.activeVersion.Store(packaged.Version)
	if err := fixture.manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := updateflow.Load(fixture.paths.journal)
	if err != nil || recovered.Stage != updateflow.StageIdle || recovered.ActiveVersion != packaged.Version || recovered.ActiveDigest != packaged.SHA256 {
		t.Fatalf("journal=%+v err=%v", recovered, err)
	}
}

func TestOlderExecutableCannotSupersedeBlockedWorkerTransaction(t *testing.T) {
	fixture := newFixture(t)
	now := time.Now().UTC()
	newer := release("2026.08.18.4", fixture.fetcher.body)
	journal := withRelease(withActiveRelease(updateflow.Journal{
		Schema: updateflow.SchemaV1, TransactionID: "txn-blocked-no-downgrade", Stage: updateflow.StageBlocked,
		BootID: "hostd", StageUpdatedAt: now, LastFailure: updateflow.FailureDrain,
	}, newer), fixture.candidate, fixture.paths.staged)
	if err := updateflow.Write(fixture.paths.journal, journal, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Recover(context.Background()); !errors.Is(err, ErrBlocked) {
		t.Fatalf("recover error=%v, want blocked", err)
	}
	persisted, err := updateflow.Load(fixture.paths.journal)
	if err != nil || persisted.TransactionID != journal.TransactionID || persisted.Stage != updateflow.StageBlocked {
		t.Fatalf("journal=%+v err=%v", persisted, err)
	}
}

func TestRuntimeStagingPatternPreservesDarwinPackageSuffix(t *testing.T) {
	if got := runtimeStagingPattern("darwin"); got != ".paperboat-runtime-*.pkg" {
		t.Fatalf("darwin staging pattern = %q", got)
	}
	if got := runtimeStagingPattern("linux"); got != ".paperboat-runtime-*" {
		t.Fatalf("linux staging pattern = %q", got)
	}
}

func TestDarwinInstalledExecutableIsRunnableByWorker(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "installed-pb")
	destination := filepath.Join(root, "releases", "pb.staged")
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("signed executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := stageInstalledDarwinExecutable(source, destination, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("staged Darwin executable mode=%#o, want executable", info.Mode().Perm())
	}
}

func TestOneHundredWorkerUpdatesKeepOneHostdAndBoundedRetention(t *testing.T) {
	fixture := newFixture(t)
	hostd := fixture.hostd
	for generation := 2; generation <= 101; generation++ {
		candidate := release(fmt.Sprintf("2026.08.18.%d", generation), fixture.fetcher.body)
		result, err := fixture.manager.Activate(context.Background(), candidate)
		if err != nil || !result.Updated || result.Version != candidate.Version {
			t.Fatalf("generation %d: result=%+v err=%v", generation, result, err)
		}
		if fixture.hostd != hostd {
			t.Fatalf("generation %d replaced the stable hostd", generation)
		}
		entries, err := retainedFiles(fixture.paths)
		if err != nil {
			t.Fatalf("generation %d: %v", generation, err)
		}
		if entries != 2 {
			t.Fatalf("generation %d retained %d artifacts, want one runtime current+rollback", generation, entries)
		}
	}
	if fixture.hostd.activations != 100 || fixture.starter.starts != 100 {
		t.Fatalf("activations=%d starts=%d", fixture.hostd.activations, fixture.starter.starts)
	}
}

func retainedFiles(paths fixturePaths) (int, error) {
	count := 0
	for _, directory := range []string{filepath.Dir(paths.current), filepath.Dir(paths.rollback), filepath.Dir(paths.staged)} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return 0, err
		}
		count += len(entries)
	}
	return count, nil
}

func TestWorkerUpdateRollsBackWithoutRestartingHostd(t *testing.T) {
	fixture := newFixture(t)
	// Different releases must exercise actual file restoration, not merely
	// two version labels for identical test-executable bytes.
	fixture.fetcher.body = append(append([]byte(nil), fixture.fetcher.body...), []byte("candidate release")...)
	fixture.candidate = release(fixture.candidate.Version, fixture.fetcher.body)
	if err := os.WriteFile(filepath.Join(fixture.paths.root, "installed", "pb"), fixture.fetcher.body, 0700); err != nil {
		t.Fatal(err)
	}

	fixture.health.err = errors.New("relay unavailable")
	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if err == nil || err.Error() != "relay unavailable" {
		t.Fatalf("error=%v", err)
	}
	if fixture.hostd.activations != 2 || fixture.hostd.active.WorkerID != workerID(fixture.active.Version) {
		t.Fatalf("hostd=%+v", fixture.hostd)
	}
	if fixture.starter.starts != 2 {
		t.Fatalf("starts=%d", fixture.starter.starts)
	}
	if !regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256) || !regularMatches(fixture.paths.staged, fixture.candidate.Length, fixture.candidate.SHA256) {
		t.Fatal("rollback did not retain old active and quarantine candidate")
	}
	journal, loadErr := updateflow.Load(fixture.paths.journal)
	if loadErr != nil || journal.Stage != updateflow.StageIdle || journal.LastFailure != updateflow.FailureHealth || journal.CandidateVersion != fixture.candidate.Version {
		t.Fatalf("journal=%+v err=%v", journal, loadErr)
	}
	if _, err := fixture.manager.Activate(context.Background(), fixture.candidate); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("same candidate err=%v, want quarantine", err)
	}
}

func TestWorkerUpdateRefusesCutoverWithoutAuthorizedRecovery(t *testing.T) {
	fixture := newFixture(t)
	fixture.fetcher.recoveryError = ErrReleaseRevoked
	result, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if !errors.Is(err, ErrReleaseRevoked) || result.Updated {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if fixture.hostd.activations != 0 || fixture.hostd.active.WorkerID != workerID(fixture.active.Version) {
		t.Fatalf("running worker changed: %+v", fixture.hostd)
	}
	if !regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256) {
		t.Fatal("active executable changed after recovery authorization failed")
	}
}

func TestWorkerUpdateReverifiesRollbackBytesBeforeStart(t *testing.T) {
	fixture := newFixture(t)
	fixture.health.check = func() {
		if err := os.WriteFile(fixture.paths.rollback, []byte("wrong rollback bytes"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.health.err = errors.New("candidate unhealthy")
	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if !errors.Is(err, ErrInvalidRelease) || !errors.Is(err, ErrBlocked) {
		t.Fatalf("error=%v, want invalid release and blocked", err)
	}
	if fixture.starter.starts != 1 || fixture.hostd.activations != 1 {
		t.Fatalf("unverified rollback started: starts=%d activations=%d", fixture.starter.starts, fixture.hostd.activations)
	}
}

func TestWorkerUpdateAuthorizesSuccessfulRollback(t *testing.T) {
	fixture := newFixture(t)
	fixture.health.err = errors.New("candidate unhealthy")
	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if err == nil || fixture.fetcher.recoveryCalls != 4 {
		t.Fatalf("error=%v recovery calls=%d", err, fixture.fetcher.recoveryCalls)
	}
	for _, version := range fixture.fetcher.recoveryVersions {
		if version != fixture.active.Version {
			t.Fatalf("authorized version=%q want=%q", version, fixture.active.Version)
		}
	}
}

func TestWorkerUpdateRecoveryAuthorizationHonorsCancellation(t *testing.T) {
	fixture := newFixture(t)
	fixture.fetcher.recovery = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := fixture.manager.Activate(ctx, fixture.candidate)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled recovery authorization took %s", elapsed)
	}
	if fixture.hostd.activations != 0 {
		t.Fatalf("activation occurred after cancellation")
	}
}

func TestWorkerUpdateLeavesUncertainCutoverForRecovery(t *testing.T) {
	fixture := newFixture(t)
	fixture.starter.activateError = errors.New("activation response lost")
	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if err == nil || err.Error() != "activation response lost" {
		t.Fatalf("error=%v", err)
	}
	journal, loadErr := updateflow.Load(fixture.paths.journal)
	if loadErr != nil || journal.Stage != updateflow.StageCutover {
		t.Fatalf("journal=%+v err=%v", journal, loadErr)
	}
	// The fake hostd did activate the candidate before its response was lost.
	fixture.starter.activateError = nil
	if err := fixture.manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.manager.ActiveVersion() != fixture.candidate.Version || fixture.hostd.active.WorkerID != workerID(fixture.candidate.Version) {
		t.Fatalf("version=%s hostd=%+v", fixture.manager.ActiveVersion(), fixture.hostd)
	}
}

func TestCrashRecoveryRechecksRollbackAuthorization(t *testing.T) {
	fixture := newFixture(t)
	fixture.starter.activateError = errors.New("activation response lost")
	if _, err := fixture.manager.Activate(context.Background(), fixture.candidate); err == nil {
		t.Fatal("activation unexpectedly succeeded")
	}
	fixture.starter.activateError = nil
	fixture.health.err = errors.New("candidate unhealthy after restart")
	fixture.fetcher.recoveryError = ErrReleaseRevoked
	if err := fixture.manager.Recover(context.Background()); !errors.Is(err, ErrReleaseRevoked) || !errors.Is(err, ErrBlocked) {
		t.Fatalf("recover error=%v, want revoked blocked recovery", err)
	}
	if fixture.starter.starts != 1 {
		t.Fatalf("revoked rollback started during crash recovery: starts=%d", fixture.starter.starts)
	}
	if fixture.hostd.active.WorkerID != workerID(fixture.candidate.Version) {
		t.Fatalf("recovery guessed a different running worker: %+v", fixture.hostd.active)
	}
}

func TestWorkerUpdateRejectsTamperedArtifactBeforeCandidateStart(t *testing.T) {
	fixture := newFixture(t)
	fixture.fetcher.body = []byte("tampered")
	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("error=%v", err)
	}
	if fixture.starter.starts != 0 || fixture.hostd.activations != 0 {
		t.Fatalf("candidate started after failed verification: starts=%d activations=%d", fixture.starter.starts, fixture.hostd.activations)
	}
	journal, loadErr := updateflow.Load(fixture.paths.journal)
	if loadErr != nil || journal.Stage != updateflow.StageIdle || journal.LastFailure != updateflow.FailureVerification || journal.ActiveVersion != fixture.active.Version {
		t.Fatalf("journal=%+v err=%v", journal, loadErr)
	}
	if recoverErr := fixture.manager.Recover(context.Background()); recoverErr != nil {
		t.Fatalf("failed verification stranded restart recovery: %v", recoverErr)
	}
}

func TestWorkerUpdateRejectsNativeSignatureBeforeCandidateStart(t *testing.T) {
	fixture := newFixture(t)
	calls := 0
	fixture.manager.config.NativeVerifier = nativeVerifierFunc(func(_ context.Context, path, platform, architecture string) error {
		calls++
		if filepath.Dir(path) != filepath.Dir(fixture.paths.staged) || filepath.Base(path) == filepath.Base(fixture.paths.staged) || platform != runtime.GOOS || architecture != runtime.GOARCH {
			t.Fatalf("native verification input path=%q platform=%q architecture=%q", path, platform, architecture)
		}
		return errors.New("native signature rejected")
	})
	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("error=%v", err)
	}
	if calls != 1 || fixture.starter.starts != 0 || fixture.hostd.activations != 0 {
		t.Fatalf("calls=%d starts=%d activations=%d", calls, fixture.starter.starts, fixture.hostd.activations)
	}
}

func TestWorkerUpdateRejectsUnsignedCompatibilityRange(t *testing.T) {
	fixture := newFixture(t)
	invalid := fixture.candidate
	invalid.HostdAPIMin, invalid.HostdAPIMax = 0, 0
	_, err := fixture.manager.Activate(context.Background(), invalid)
	if !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("error=%v", err)
	}
	if fixture.starter.starts != 0 {
		t.Fatalf("candidate started with an invalid API range")
	}
}

func TestSchedulerAdapterOnlyRunsSafeManagerTransaction(t *testing.T) {
	fixture := newFixture(t)
	scheduler, err := fixture.manager.MandatoryScheduler(func(context.Context) (Release, bool, error) {
		return fixture.candidate, true, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := scheduler.CheckNow(context.Background())
	if err != nil || !result.Updated || fixture.hostd.activations != 1 {
		t.Fatalf("result=%+v err=%v activations=%d", result, err, fixture.hostd.activations)
	}
}

func TestActiveTerminalBusyRestoresOldWorkerWithoutQuarantine(t *testing.T) {
	fixture := newFixture(t)
	busy := &autoupdate.ActiveTerminalSessionsError{RequiredVersion: fixture.candidate.Version}
	gate := &busyActivationGate{drainErr: busy}
	fixture.manager.config.Gate = gate

	result, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	var gotBusy *autoupdate.ActiveTerminalSessionsError
	if !errors.As(err, &gotBusy) || gotBusy.RequiredVersion != fixture.candidate.Version {
		t.Fatalf("error=%v", err)
	}
	if result.Updated || result.Version != fixture.active.Version {
		t.Fatalf("result=%+v", result)
	}
	if gate.rollbacks != 1 {
		t.Fatalf("rollbacks=%d want 1", gate.rollbacks)
	}
	if _, err := os.Stat(fixture.paths.staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged candidate remains: %v", err)
	}
	state, err := fixture.manager.TransactionState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != updateflow.StageIdle || state.Quarantined || state.CandidateVersion != "" {
		t.Fatalf("state=%+v", state)
	}
	journal, err := updateflow.Load(fixture.paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	if journal.BlockedReason != autoupdate.BlockedActiveTerminalSessions || journal.RequiredVersion != fixture.candidate.Version || journal.NextCheckAt.IsZero() {
		t.Fatalf("blocked journal=%+v", journal)
	}
	if journal.LastFailure != updateflow.FailureNone || journal.RollbackCount != 0 {
		t.Fatalf("expected busy recorded as failed rollback: %+v", journal)
	}
}

func TestActiveTerminalBusyDoesNotHideRollbackFailure(t *testing.T) {
	fixture := newFixture(t)
	rollbackErr := errors.New("gate rollback failed")
	gate := &busyActivationGate{
		drainErr:    &autoupdate.ActiveTerminalSessionsError{RequiredVersion: fixture.candidate.Version},
		rollbackErr: rollbackErr,
	}
	fixture.manager.config.Gate = gate

	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	var busy *autoupdate.ActiveTerminalSessionsError
	if !errors.Is(err, rollbackErr) || !errors.Is(err, ErrBlocked) || errors.As(err, &busy) {
		t.Fatalf("error=%v", err)
	}
}

func TestNativeRuntimeActivationAdoptsRestartedHostdFence(t *testing.T) {
	fixture := newFixture(t)
	var activated []string
	var monitoring updateflow.Journal
	fixture.manager.config.ActivateRuntime = func(_ context.Context, version string) (hostdproto.Status, error) {
		activated = append(activated, version)
		status := hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(version), APIVersion: 1, Epoch: 41}
		fixture.hostd.active = status
		return status, nil
	}
	fixture.manager.config.WriteJournal = func(path string, journal updateflow.Journal, uid, gid int) error {
		if journal.Stage == updateflow.StageMonitoring {
			monitoring = journal
		}
		return updateflow.Write(path, journal, uid, gid)
	}

	result, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if err != nil || !result.Updated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(activated) != 1 || activated[0] != fixture.candidate.Version || monitoring.WorkerID != workerID(fixture.candidate.Version) || monitoring.WorkerEpoch != 41 {
		t.Fatalf("activated=%v monitoring=%+v", activated, monitoring)
	}
	if fixture.hostd.activations != 0 {
		t.Fatal("used stale candidate IPC after native host restart")
	}
	if fixture.starter.stops != 1 {
		t.Fatalf("candidate stops=%d", fixture.starter.stops)
	}
}

func TestNativeRuntimeHealthyRollbackRestartsRestoredCanonicalHostd(t *testing.T) {
	fixture := newFixture(t)
	fixture.health.err = errors.New("candidate unhealthy")
	var activated []string
	fixture.manager.config.ActivateRuntime = func(_ context.Context, version string) (hostdproto.Status, error) {
		activated = append(activated, version)
		status := hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(version), APIVersion: 1, Epoch: uint64(len(activated) + 10)}
		fixture.hostd.active = status
		return status, nil
	}
	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if err == nil || errors.Is(err, ErrBlocked) {
		t.Fatalf("error=%v", err)
	}
	if len(activated) != 2 || activated[0] != fixture.candidate.Version || activated[1] != fixture.active.Version || fixture.hostd.activations != 0 {
		t.Fatalf("activated=%v stale activations=%d", activated, fixture.hostd.activations)
	}
}

func TestNativeRuntimeBusyDoesNotRestartHostd(t *testing.T) {
	fixture := newFixture(t)
	fixture.manager.config.Gate = &busyActivationGate{drainErr: &autoupdate.ActiveTerminalSessionsError{RequiredVersion: fixture.candidate.Version}}
	calls := 0
	fixture.manager.config.ActivateRuntime = func(context.Context, string) (hostdproto.Status, error) {
		calls++
		return hostdproto.Status{}, nil
	}
	if _, err := fixture.manager.Activate(context.Background(), fixture.candidate); err == nil {
		t.Fatal("busy activation succeeded")
	}
	if calls != 0 {
		t.Fatalf("hostd restarts=%d", calls)
	}
}

func TestNativeRuntimeCrashAtCutoverRestoresSignedPreviousHostd(t *testing.T) {
	fixture := newFixture(t)
	fixture.starter.activateError = errors.New("activation response lost")
	if _, err := fixture.manager.Activate(context.Background(), fixture.candidate); err == nil {
		t.Fatal("cutover did not remain interrupted")
	}
	fixture.hostd.activeErr = errors.New("new hostd unavailable")
	var activated []string
	fixture.manager.config.ActivateRuntime = func(_ context.Context, version string) (hostdproto.Status, error) {
		activated = append(activated, version)
		status := hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(version), APIVersion: 1, Epoch: 91}
		fixture.hostd.active, fixture.hostd.activeErr = status, nil
		return status, nil
	}
	if err := fixture.manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(activated) != 1 || activated[0] != fixture.active.Version || !regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256) {
		t.Fatalf("activated=%v previous restored=%v", activated, regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256))
	}
}

func TestNativeRuntimeCutoverHookIsBoundedAndFailureRestoresPrevious(t *testing.T) {
	fixture := newFixture(t)
	fixture.manager.config.RollbackTimeout = 25 * time.Millisecond
	var activated []string
	var forwardAllowance time.Duration
	fixture.manager.config.ActivateRuntime = func(ctx context.Context, version string) (hostdproto.Status, error) {
		activated = append(activated, version)
		if version == fixture.candidate.Version {
			deadline, _ := ctx.Deadline()
			forwardAllowance = time.Until(deadline)
			<-ctx.Done()
			return hostdproto.Status{}, ctx.Err()
		}
		status := hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(version), APIVersion: 1, Epoch: 92}
		fixture.hostd.active = status
		return status, nil
	}
	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrBlocked) {
		t.Fatalf("error=%v", err)
	}
	if forwardAllowance <= 0 || forwardAllowance > 50*time.Millisecond {
		t.Fatalf("forward hook allowance=%v", forwardAllowance)
	}
	if len(activated) != 2 || activated[0] != fixture.candidate.Version || activated[1] != fixture.active.Version || !regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256) {
		t.Fatalf("activated=%v previous restored=%v", activated, regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256))
	}
}

func TestNativeRuntimeCandidateStopFailureRestoresPrevious(t *testing.T) {
	fixture := newFixture(t)
	stopErr := errors.New("candidate did not exit")
	fixture.starter.stopError = stopErr
	var activated []string
	fixture.manager.config.ActivateRuntime = func(_ context.Context, version string) (hostdproto.Status, error) {
		activated = append(activated, version)
		status := hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(version), APIVersion: 1, Epoch: 93}
		fixture.hostd.active = status
		return status, nil
	}
	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if !errors.Is(err, stopErr) || !errors.Is(err, ErrBlocked) {
		t.Fatalf("error=%v", err)
	}
	if len(activated) != 1 || activated[0] != fixture.active.Version || fixture.hostd.activations != 0 || !regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256) {
		t.Fatalf("activated=%v stale activations=%d previous restored=%v", activated, fixture.hostd.activations, regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256))
	}
}

func TestNativeRuntimeParentCancellationStillRestoresPrevious(t *testing.T) {
	fixture := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	var activated []string
	fixture.manager.config.ActivateRuntime = func(hookCtx context.Context, version string) (hostdproto.Status, error) {
		activated = append(activated, version)
		if version == fixture.candidate.Version {
			cancel()
			return hostdproto.Status{}, context.Canceled
		}
		if hookCtx.Err() != nil {
			return hostdproto.Status{}, hookCtx.Err()
		}
		status := hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(version), APIVersion: 1, Epoch: 94}
		fixture.hostd.active = status
		return status, nil
	}
	_, err := fixture.manager.Activate(ctx, fixture.candidate)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrBlocked) {
		t.Fatalf("error=%v", err)
	}
	if len(activated) != 2 || activated[1] != fixture.active.Version || !regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256) {
		t.Fatalf("activated=%v previous restored=%v", activated, regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256))
	}
}

func TestNativeRuntimePromotedJournalFailureStopsCandidateBeforeRestore(t *testing.T) {
	fixture := newFixture(t)
	writeErr := errors.New("promoted journal unavailable")
	failed := false
	fixture.manager.config.WriteJournal = func(path string, journal updateflow.Journal, uid, gid int) error {
		if !failed && journal.Stage == updateflow.StageCutover && journal.StagedPath == fixture.paths.current {
			failed = true
			return writeErr
		}
		return updateflow.Write(path, journal, uid, gid)
	}
	var activated []string
	fixture.manager.config.ActivateRuntime = func(_ context.Context, version string) (hostdproto.Status, error) {
		activated = append(activated, version)
		status := hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(version), APIVersion: 1, Epoch: 95}
		fixture.hostd.active = status
		return status, nil
	}
	_, err := fixture.manager.Activate(context.Background(), fixture.candidate)
	if !errors.Is(err, writeErr) || errors.Is(err, ErrBlocked) {
		t.Fatalf("error=%v", err)
	}
	if fixture.starter.stops != 1 || len(activated) != 1 || activated[0] != fixture.active.Version || fixture.hostd.activations != 0 {
		t.Fatalf("stops=%d activated=%v stale activations=%d", fixture.starter.stops, activated, fixture.hostd.activations)
	}
}

type fixturePaths struct{ root, current, rollback, staged, journal string }
type fixture struct {
	manager           *Manager
	paths             fixturePaths
	active, candidate Release
	fetcher           *fakeFetcher
	starter           *fakeStarter
	hostd             *fakeHostd
	health            *fakeHealth
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"current", "rollback", "staged", "state", "installed"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	paths := fixturePaths{root: root, current: filepath.Join(root, "current", "pb"), rollback: filepath.Join(root, "rollback", "pb"), staged: filepath.Join(root, "staged", "pb"), journal: filepath.Join(root, "state", "journal.json")}
	if err := os.WriteFile(paths.current, body, 0o700); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(root, "installed", "pb")
	if err := os.WriteFile(installed, body, 0o700); err != nil {
		t.Fatal(err)
	}
	active := release("2026.08.18.1", body)
	candidate := release("2026.08.18.2", body)
	hostd := &fakeHostd{active: hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(active.Version), APIVersion: 1, Epoch: 1}}
	starter := &fakeStarter{hostd: hostd}
	health := &fakeHealth{}
	fetcher := &fakeFetcher{body: body}
	workerUID := os.Geteuid()
	if workerUID == 0 {
		workerUID = 1
	}
	manager, err := New(Config{StatePath: paths.journal, Binary: paths.current, BinaryRollback: paths.rollback, BinaryStaged: paths.staged, Active: active, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), WorkerUID: workerUID, WorkerGID: os.Getegid(), HostdEndpoint: "private-hostd", Capability: bytes.Repeat([]byte{1}, 32), Fetcher: fetcher, Starter: starter, Hostd: hostd, Health: health, Gate: fakeActivationGate{}, NativeVerifier: nativeVerifierFunc(func(context.Context, string, string, string) error { return nil }), ExtractPackage: func(context.Context, string, string) (string, error) { return installed, nil }, MonitorWindow: time.Millisecond, HealthInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{manager: manager, paths: paths, active: active, candidate: candidate, fetcher: fetcher, starter: starter, hostd: hostd, health: health}
}

func release(version string, body []byte) Release {
	sum := sha256.Sum256(body)
	return Release{Version: version, SHA256: hex.EncodeToString(sum[:]), Length: int64(len(body)), Platform: runtime.GOOS, Architecture: runtime.GOARCH, HostdAPIMin: 1, HostdAPIMax: 2, RuntimeAPIMin: 1, RuntimeAPIMax: 2}
}

func TestValidWorkerIdentitySupportsExactRootEnrollment(t *testing.T) {
	for _, test := range []struct {
		uid, gid int
		want     bool
	}{
		{uid: 1000, gid: 1000, want: true},
		{uid: 0, gid: 0, want: true},
		{uid: 0, gid: 1000},
		{uid: 1000, gid: 0},
		{uid: -1, gid: -1},
	} {
		if got := validWorkerIdentity(test.uid, test.gid); got != test.want {
			t.Fatalf("validWorkerIdentity(%d, %d)=%v want %v", test.uid, test.gid, got, test.want)
		}
	}
}

type fakeFetcher struct {
	body             []byte
	recoveryError    error
	recoveryCalls    int
	recoveryVersions []string
	recovery         func(context.Context) error
}

func (f *fakeFetcher) Fetch(context.Context, Release) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

func (f *fakeFetcher) AuthorizeRecovery(ctx context.Context, version, _, _ string) error {
	f.recoveryCalls++
	f.recoveryVersions = append(f.recoveryVersions, version)
	if f.recovery != nil {
		return f.recovery(ctx)
	}
	return f.recoveryError
}

type fakeHostd struct {
	active      hostdproto.Status
	activeErr   error
	activations int
}

func (h *fakeHostd) Active(context.Context) (hostdproto.Status, error) { return h.active, h.activeErr }

type fakeStarter struct {
	hostd         *fakeHostd
	starts        int
	requests      []StartRequest
	activateError error
	stopError     error
	stops         int
}

func (s *fakeStarter) Start(_ context.Context, request StartRequest) (Worker, error) {
	s.starts++
	s.requests = append(s.requests, request)
	return &fakeWorker{starter: s, request: request}, nil
}

type fakeWorker struct {
	starter *fakeStarter
	request StartRequest
	epoch   uint64
}

func (w *fakeWorker) Ready(context.Context) (hostdproto.Status, error) {
	w.epoch = w.starter.hostd.active.Epoch + 1
	return hostdproto.Status{State: hostdproto.StateCandidate, WorkerID: w.request.WorkerID, APIVersion: 1, Epoch: w.epoch}, nil
}
func (w *fakeWorker) Activate(context.Context) (hostdproto.Status, error) {
	w.starter.hostd.active = hostdproto.Status{State: hostdproto.StateActive, WorkerID: w.request.WorkerID, APIVersion: 1, Epoch: w.epoch}
	w.starter.hostd.activations++
	if w.starter.activateError != nil {
		return hostdproto.Status{}, w.starter.activateError
	}
	return w.starter.hostd.active, nil
}
func (w *fakeWorker) Stop(context.Context) error {
	w.starter.stops++
	return w.starter.stopError
}

type fakeHealth struct {
	err   error
	check func()
}

func (h *fakeHealth) Check(context.Context, hostdproto.Status, Release) error {
	if h.check != nil {
		h.check()
	}
	return h.err
}

type fakeActivationGate struct{}

func (fakeActivationGate) Candidate(context.Context, GateRequest) error { return nil }
func (fakeActivationGate) Drain(context.Context, GateRequest) error     { return nil }
func (fakeActivationGate) Active(context.Context, GateRequest) error    { return nil }
func (fakeActivationGate) Commit(context.Context, GateRequest) error    { return nil }
func (fakeActivationGate) Rollback(context.Context, GateRequest) error  { return nil }

type busyActivationGate struct {
	drainErr    error
	rollbackErr error
	rollbacks   int
}

func (*busyActivationGate) Candidate(context.Context, GateRequest) error { return nil }
func (g *busyActivationGate) Drain(context.Context, GateRequest) error   { return g.drainErr }
func (*busyActivationGate) Active(context.Context, GateRequest) error    { return nil }
func (*busyActivationGate) Commit(context.Context, GateRequest) error    { return nil }
func (g *busyActivationGate) Rollback(context.Context, GateRequest) error {
	g.rollbacks++
	return g.rollbackErr
}

type blockingHealth struct {
	entered chan struct{}
	release chan struct{}
}

func (h *blockingHealth) Check(context.Context, hostdproto.Status, Release) error {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-h.release
	return nil
}

type nativeVerifierFunc func(context.Context, string, string, string) error

func (f nativeVerifierFunc) Verify(ctx context.Context, path, platform, architecture string) error {
	return f(ctx, path, platform, architecture)
}

func TestCanceledHealthCheckRestoresPolicyValidInstallation(t *testing.T) {
	fixture := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.health.check = cancel
	fixture.health.err = context.Canceled
	fixture.fetcher.recovery = func(ctx context.Context) error { return ctx.Err() }
	_, err := fixture.manager.Activate(ctx, fixture.candidate)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrBlocked) {
		t.Fatalf("error=%v", err)
	}
	if fixture.hostd.active.WorkerID != workerID(fixture.active.Version) || !regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256) {
		t.Fatal("canceled health check did not recover the permitted previous installation")
	}
	journal, loadErr := updateflow.Load(fixture.paths.journal)
	if loadErr != nil || journal.Stage != updateflow.StageIdle {
		t.Fatalf("journal=%+v error=%v", journal, loadErr)
	}
}
