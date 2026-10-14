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
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

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
		_, err := activatePrepared(context.Background(), fixture.manager, fixture.candidate)
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
	state, err := fixture.manager.TransactionState()
	if err != nil || state.Stage != updateflow.StageIdle || state.ActiveVersion != fixture.active.Version {
		t.Fatalf("pre-seed state=%+v err=%v", state, err)
	}
	if err := fixture.manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	journal, err := updateflow.Load(fixture.paths.journal)
	if err != nil || journal.Stage != updateflow.StageIdle || journal.ActiveVersion != fixture.active.Version || journal.ActiveDigest != fixture.active.SHA256 || journal.ActiveLength != fixture.active.Length || journal.ActiveSource != fixture.active.LocalSource {
		t.Fatalf("journal=%+v err=%v", journal, err)
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

func TestWorkerUpdateRefusesCutoverWithoutAuthorizedRecovery(t *testing.T) {
	fixture := newFixture(t)
	fixture.fetcher.recoveryError = ErrReleaseRevoked
	result, err := activatePrepared(context.Background(), fixture.manager, fixture.candidate)
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

func TestWorkerUpdateRecoveryAuthorizationHonorsCancellation(t *testing.T) {
	fixture := newFixture(t)
	fixture.fetcher.recovery = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := activatePrepared(ctx, fixture.manager, fixture.candidate)
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

func TestWorkerUpdateRejectsTamperedArtifactBeforeCandidateStart(t *testing.T) {
	fixture := newFixture(t)
	fixture.fetcher.body = []byte("tampered")
	_, err := activatePrepared(context.Background(), fixture.manager, fixture.candidate)
	if !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("error=%v", err)
	}
	if *fixture.nativeCalls != 0 || fixture.hostd.activations != 0 {
		t.Fatalf("candidate started after failed verification: starts=%d activations=%d", *fixture.nativeCalls, fixture.hostd.activations)
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
	_, err := activatePrepared(context.Background(), fixture.manager, fixture.candidate)
	if !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("error=%v", err)
	}
	if calls != 1 || *fixture.nativeCalls != 0 || fixture.hostd.activations != 0 {
		t.Fatalf("calls=%d starts=%d activations=%d", calls, *fixture.nativeCalls, fixture.hostd.activations)
	}
}

func TestWorkerUpdateRejectsUnsignedCompatibilityRange(t *testing.T) {
	fixture := newFixture(t)
	invalid := fixture.candidate
	invalid.HostdAPIMin, invalid.HostdAPIMax = 0, 0
	_, err := activatePrepared(context.Background(), fixture.manager, invalid)
	if !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("error=%v", err)
	}
	if *fixture.nativeCalls != 0 {
		t.Fatalf("candidate started with an invalid API range")
	}
}

func TestSchedulerAdapterOnlyReportsAvailableUpdate(t *testing.T) {
	fixture := newFixture(t)
	scheduler, err := fixture.manager.MandatoryScheduler(func(context.Context) (Release, bool, error) {
		return fixture.candidate, true, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := scheduler.CheckNow(context.Background())
	if err != nil || result.Updated || result.Version != fixture.candidate.Version || fixture.hostd.activations != 0 || *fixture.nativeCalls != 0 {
		t.Fatalf("result=%+v err=%v activations=%d", result, err, fixture.hostd.activations)
	}
}

type fixturePaths struct{ root, current, rollback, staged, journal string }
type fixture struct {
	manager           *Manager
	paths             fixturePaths
	active, candidate Release
	fetcher           *fakeFetcher
	nativeCalls       *int
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
	nativeCalls := 0
	health := &fakeHealth{}
	fetcher := &fakeFetcher{body: body}
	var manager *Manager
	var committed *installsource.Source
	apply := func(ctx context.Context, a PreparedActivation) (installsource.Source, error) {
		nativeCalls++
		if committed != nil && *committed == a.Source {
			return a.Source, a.Validate(ctx)
		}
		distribution := installsource.Official
		if manager.active.LocalSource != nil {
			distribution = manager.active.LocalSource.Distribution
		}
		prior, err := installsource.Inspect(paths.current, manager.active.Version, distribution)
		if err != nil {
			return installsource.Source{}, err
		}
		if manager.active.LocalSource != nil {
			prior.AutomaticUpdates = manager.active.LocalSource.AutomaticUpdates
		}
		if err = a.Prepare(ctx); err != nil {
			return prior, err
		}
		before, err := os.ReadFile(paths.current)
		if err != nil {
			return installsource.Source{}, err
		}
		next, err := os.ReadFile(a.Executable)
		if err != nil {
			return installsource.Source{}, err
		}
		baseline := hostd.active
		if err = os.WriteFile(paths.current, next, 0700); err != nil {
			return installsource.Source{}, err
		}
		hostd.active = hostdproto.Status{State: hostdproto.StateActive, WorkerID: workerID(a.Source.Version), APIVersion: 1, Epoch: baseline.Epoch + 1}
		if err = a.Validate(ctx); err != nil {
			if restoreErr := os.WriteFile(paths.current, before, 0700); restoreErr != nil {
				return installsource.Source{}, errors.Join(err, restoreErr)
			}
			hostd.active = baseline
			return prior, err
		}
		selected := a.Source
		committed = &selected
		return a.Source, nil
	}
	manager, err = New(Config{ApplyPrepared: apply, StatePath: paths.journal, Binary: paths.current, BinaryStaged: paths.staged, Active: active, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Fetcher: fetcher, Hostd: hostd, Health: health, Gate: fakeActivationGate{}, NativeVerifier: nativeVerifierFunc(func(context.Context, string, string, string) error { return nil }), ExtractPackage: func(context.Context, string, string) (string, error) { return installed, nil }, MonitorWindow: time.Millisecond, HealthInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{manager: manager, paths: paths, active: active, candidate: candidate, fetcher: fetcher, nativeCalls: &nativeCalls, hostd: hostd, health: health}
}

func TestJournalMissingOnlyRequiresPureBoundedAbsence(t *testing.T) {
	missing := &os.PathError{Op: "open", Path: filepath.Join("private", "journal.json"), Err: os.ErrNotExist}
	if !journalMissingOnly(fmt.Errorf("safe read wrapper: %w", missing)) {
		t.Fatal("wrapped missing-file cause was not recognized")
	}
	if journalMissingOnly(errors.Join(missing, errors.New("independent close failure"))) {
		t.Fatal("joined operational failure was treated as a missing journal")
	}
	var typedNil *os.PathError
	if journalMissingOnly(typedNil) {
		t.Fatal("typed nil error was treated as a missing journal")
	}
	if journalMissingOnly(cyclicJournalCause{}) {
		t.Fatal("cyclic cause was treated as a missing journal")
	}
}

type cyclicJournalCause struct{}

func (cyclicJournalCause) Error() string       { return "cyclic journal cause" }
func (cause cyclicJournalCause) Unwrap() error { return cause }

func TestJournalReadFailureBlocksRecoveryAndRetainsCause(t *testing.T) {
	fixture := newFixture(t)
	currentBefore, err := os.ReadFile(fixture.paths.current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.paths.journal, 0o700); err != nil {
		t.Fatal(err)
	}

	state, stateErr := fixture.manager.TransactionState()
	var pathErr *os.PathError
	if stateErr == nil || errors.Is(stateErr, updateflow.ErrInvalidJournal) || errors.Is(stateErr, os.ErrNotExist) || !errors.As(stateErr, &pathErr) || pathErr.Path != fixture.paths.journal {
		t.Fatalf("state=%+v error=%v, want original journal read cause", state, stateErr)
	}
	if strings.Contains(stateErr.Error(), fixture.paths.journal) {
		t.Fatalf("state error exposed journal path: %q", stateErr)
	}

	recoveryErr := fixture.manager.Recover(context.Background())
	pathErr = nil
	if !errors.Is(recoveryErr, ErrBlocked) || !errors.As(recoveryErr, &pathErr) || pathErr.Path != fixture.paths.journal {
		t.Fatalf("recovery error=%v, want blocked plus original read cause", recoveryErr)
	}
	if strings.Contains(recoveryErr.Error(), fixture.paths.journal) {
		t.Fatalf("recovery error exposed journal path: %q", recoveryErr)
	}
	var quarantined string
	var quarantineErr error
	func() {
		fixture.manager.mu.Lock()
		defer fixture.manager.mu.Unlock()
		quarantined, quarantineErr = fixture.manager.quarantinedLocked()
	}()
	pathErr = nil
	if quarantined != "" || !errors.Is(quarantineErr, ErrBlocked) || errors.Is(quarantineErr, os.ErrNotExist) || !errors.As(quarantineErr, &pathErr) || pathErr.Path != fixture.paths.journal {
		t.Fatalf("quarantine=%q error=%v, want blocked plus original journal read cause", quarantined, quarantineErr)
	}
	currentAfter, err := os.ReadFile(fixture.paths.current)
	if err != nil || !bytes.Equal(currentAfter, currentBefore) || *fixture.nativeCalls != 0 || fixture.hostd.activations != 0 {
		t.Fatalf("journal read failure changed runtime: err=%v equal=%v starts=%d activations=%d", err, bytes.Equal(currentAfter, currentBefore), *fixture.nativeCalls, fixture.hostd.activations)
	}
}

func release(version string, body []byte) Release {
	sum := sha256.Sum256(body)
	return Release{Version: version, SHA256: hex.EncodeToString(sum[:]), Length: int64(len(body)), Platform: runtime.GOOS, Architecture: runtime.GOARCH, HostdAPIMin: 1, HostdAPIMax: 2, RuntimeAPIMin: 1, RuntimeAPIMax: 2}
}

type fakeFetcher struct {
	fetchCalls       int
	body             []byte
	recoveryError    error
	recoveryCalls    int
	recoveryVersions []string
	recovery         func(context.Context) error
}

func (f *fakeFetcher) Fetch(context.Context, Release) (io.ReadCloser, error) {
	f.fetchCalls++
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
	active          hostdproto.Status
	activeErr       error
	activations     int
	stopActiveCalls int
	stopActiveError error
}

func (h *fakeHostd) Active(context.Context) (hostdproto.Status, error) { return h.active, h.activeErr }

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

// activatePrepared exercises explicit user authorization in the existing
// activation/recovery scenarios, preserving preparation failure assertions.
func activatePrepared(ctx context.Context, manager *Manager, release Release) (Result, error) {
	candidate, err := manager.Prepare(ctx, release)
	if err != nil {
		return Result{Version: manager.ActiveVersion()}, err
	}
	if candidate.ID == "" {
		return manager.Activate(ctx, release)
	}
	if err = manager.Approve(ctx, candidate.ID); err != nil {
		return Result{Version: manager.ActiveVersion()}, err
	}
	return manager.Activate(ctx, release)
}

func (h *fakeHostd) StopActive(context.Context) error {
	h.stopActiveCalls++
	err := h.stopActiveError
	h.stopActiveError = nil
	return err
}
