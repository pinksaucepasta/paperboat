//go:build darwin || linux

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

var ErrActivationPending = errors.New("verified update activation is owned by the persistent recovery helper")
var ErrParticipantReadiness = errors.New("updated installation readiness unavailable")

type UnixParticipantProbe struct {
	Version                     string `json:"version"`
	UpdaterVersion              string `json:"updater_version"`
	State                       string `json:"state"`
	Machines                    uint32 `json:"machines"`
	Running                     bool   `json:"running"`
	ControlPlaneUnavailableOnly bool   `json:"control_plane_unavailable_only"`
}

type UnixParticipants interface {
	Probe(context.Context) (UnixParticipantProbe, error)
	Restart(context.Context) error
}

// UnixActivationController operates fixed native jobs, never a release-selected
// command. Install registers the verified previous executable as a durable job.
type UnixActivationController interface {
	Install(context.Context, string, map[string]string) error
	Retire(context.Context) error
	RestartUpdater(context.Context) error
	RestartHostd(context.Context) error
}

type unixActivationHandoff struct {
	Schema              string               `json:"schema"`
	Previous            workerupdate.Release `json:"previous"`
	Candidate           string               `json:"candidate"`
	Manual              bool                 `json:"manual"`
	Started             bool                 `json:"started"`
	Baseline            UnixParticipantProbe `json:"baseline"`
	RequireInventory    bool                 `json:"require_inventory"`
	RecoveryTimeout     time.Duration        `json:"recovery_timeout"`
	ParticipantsTouched bool                 `json:"participants_touched"`
	ActiveTerminalBusy  bool                 `json:"active_terminal_busy,omitempty"`
	BusyRequiredVersion string               `json:"busy_required_version,omitempty"`
	BusyNextCheckAt     time.Time            `json:"busy_next_check_at,omitempty,omitzero"`
}

const unixHandoffSchema = "paperboat.update-activation/v1"

func unixHandoffPath(root string) string { return filepath.Join(root, "activation", "handoff.json") }

func readUnixHandoff(root string) (*unixActivationHandoff, error) {
	if err := secureRoot(root); err != nil {
		return nil, err
	}
	path := unixHandoffPath(root)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = secureRoot(filepath.Dir(path)); err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64<<10 {
		return nil, ErrInvalidConfig
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var value unixActivationHandoff
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&value) != nil || decoder.Decode(&extra) != io.EOF || value.Schema != unixHandoffSchema || value.Previous.Version == "" || value.Previous.Length <= 0 || len(value.Previous.SHA256) != 64 || value.Candidate == "" || value.ActiveTerminalBusy != (value.BusyRequiredVersion != "" && !value.BusyNextCheckAt.IsZero()) {
		return nil, ErrInvalidConfig
	}
	return &value, nil
}

// UnixActivationActive lets a restarted ordinary updater initialize without
// refreshing shared TUF state while the recovery helper owns the transaction.
func UnixActivationActive(root string) (workerupdate.Release, bool, error) {
	handoff, err := readUnixHandoff(root)
	if err != nil || handoff == nil {
		return workerupdate.Release{}, false, err
	}
	active, err := unixHandoffActive(root, handoff)
	return active, true, err
}

func unixHandoffActive(root string, handoff *unixActivationHandoff) (workerupdate.Release, error) {
	journal, err := updateflow.Load(filepath.Join(root, "transaction.json"))
	if errors.Is(err, os.ErrNotExist) {
		return handoff.Previous, nil
	}
	if err != nil {
		return workerupdate.Release{}, err
	}
	if journal.Stage == updateflow.StageIdle {
		return workerupdate.ActiveReleaseFromJournal(filepath.Join(root, "transaction.json"), journal.ActiveVersion)
	}
	return handoff.Previous, nil
}

func writeUnixHandoff(root string, h *unixActivationHandoff) error {
	if _, err := secureChild(root, "activation"); err != nil {
		return err
	}
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return atomicfile.Write(unixHandoffPath(root), data, atomicfile.Options{Mode: 0600, OwnerUID: 0, OwnerGID: 0})
}

// The process lock is separate from the handoff marker: the marker survives a
// reboot; flock excludes simultaneous manager recovery and TUF cache writers.
func unixActivationLock(root string) (*os.File, error) {
	if err := secureRoot(root); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(filepath.Join(root, "activation.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "activation.lock")
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		file.Close()
		return nil, ErrInvalidConfig
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrActivationPending
		}
		return nil, err
	}
	return file, nil
}

func verifiedUnixHelper(binary, path string, release workerupdate.Release) error {
	info, err := os.Lstat(binary)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() != release.Length {
		return workerupdate.ErrInvalidRelease
	}
	input, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer input.Close()
	//paperboat:allow-source-policy atomic-replacement owner=updated reason=verified-protected-recovery-helper
	temporary, err := os.CreateTemp(filepath.Dir(path), ".helper-")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(temporary, digest), io.LimitReader(input, release.Length+1))
	if err != nil {
		return err
	}
	if n != release.Length || hex.EncodeToString(digest.Sum(nil)) != release.SHA256 {
		return workerupdate.ErrInvalidRelease
	}
	if err = temporary.Chmod(0700); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=updated reason=verified-protected-recovery-helper
	if err = os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	return syncUnixDirectory(filepath.Dir(path))
}

func syncUnixDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s *Service) queueActivation(ctx context.Context, manual bool) (workerupdate.Result, error) {
	result := workerupdate.Result{Version: s.currentManager().ActiveVersion()}
	lock, err := unixActivationLock(s.config.StateRoot)
	if errors.Is(err, ErrActivationPending) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer lock.Close()
	handoff, err := readUnixHandoff(s.config.StateRoot)
	if err != nil {
		return result, err
	}
	if handoff != nil {
		result.Version = handoff.Candidate
		if handoff.ActiveTerminalBusy {
			return result, &autoupdate.ActiveTerminalSessionsError{RequiredVersion: handoff.BusyRequiredVersion}
		}
		return result, s.config.ActivationController.Install(ctx, filepath.Join(s.config.StateRoot, "activation", "pb"), s.config.Environment)
	}
	if err = s.refreshManager(); err != nil {
		return result, err
	}
	manager := s.currentManager()
	result.Version = manager.ActiveVersion()
	resolver := s.source.Resolve
	if manual {
		resolver = s.source.ResolveManual
	}
	release, found, err := resolver(ctx)
	if err != nil || !found || release.Version == result.Version {
		return result, err
	}
	state, err := manager.TransactionState()
	if err != nil {
		return result, err
	}
	if state.Quarantined && state.CandidateVersion == release.Version {
		return result, workerupdate.ErrQuarantined
	}
	if err = s.source.AuthorizeRecovery(ctx, s.config.Active.Version, s.config.Active.Platform, s.config.Active.Architecture); err != nil {
		return result, err
	}
	baseline, err := s.config.Participants.Probe(ctx)
	if err != nil {
		return result, errors.Join(ErrParticipantReadiness, err)
	}
	if err = unixParticipantReady(baseline, s.config.Active.Version, baseline, baseline.Machines > 0 || baseline.State == "ready"); err != nil {
		return result, err
	}
	handoff = &unixActivationHandoff{Schema: unixHandoffSchema, Previous: s.config.Active, Candidate: release.Version, Manual: manual, RecoveryTimeout: release.RollbackTimeout, Baseline: baseline, RequireInventory: baseline.Machines > 0 || baseline.State == "ready"}
	if _, err = secureChild(s.config.StateRoot, "activation"); err != nil {
		return result, err
	}
	helper := filepath.Join(s.config.StateRoot, "activation", "pb")
	if err = verifiedUnixHelper(s.config.Binary, helper, handoff.Previous); err != nil {
		return result, fmt.Errorf("prepare verified recovery executable: %w", err)
	}
	if err = writeUnixHandoff(s.config.StateRoot, handoff); err != nil {
		return result, err
	}
	result.Version = release.Version
	return result, s.config.ActivationController.Install(ctx, helper, s.config.Environment)
}

func (s *Service) refreshManager() error {
	active, err := workerupdate.ActiveReleaseFromJournal(filepath.Join(s.config.StateRoot, "transaction.json"), buildinfo.Version)
	if errors.Is(err, os.ErrNotExist) {
		active = s.config.Active
	} else if err != nil {
		return err
	}
	manager, err := s.newManager(active)
	if err != nil {
		return err
	}
	s.managerMu.Lock()
	s.manager = manager
	s.managerMu.Unlock()
	s.config.Active = active
	return nil
}

func (s *Service) currentManager() *workerupdate.Manager {
	s.managerMu.RLock()
	defer s.managerMu.RUnlock()
	return s.manager
}

// RunActivationHelper is the only process permitted to mutate a handed-off
// worker transaction. Its executable is the policy-authorized previous release.
func (s *Service) RunActivationHelper(ctx context.Context) error {
	lock, err := unixActivationLock(s.config.StateRoot)
	if err != nil {
		return err
	}
	defer lock.Close()
	handoff, err := readUnixHandoff(s.config.StateRoot)
	if err != nil {
		return err
	}
	if handoff == nil {
		return s.config.ActivationController.Retire(ctx)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	expected := filepath.Join(s.config.StateRoot, "activation", "pb")
	if executable != expected || buildinfo.Version != handoff.Previous.Version {
		return ErrInvalidConfig
	}
	// Verify the persisted helper on every start, including native reboot retry.
	if err = verifyUnixExecutable(expected, handoff.Previous); err != nil {
		return err
	}
	gate := &unixParticipantGate{inner: s.config.ActivationGate, participants: s.config.Participants, controller: s.config.ActivationController, handoff: handoff, persist: func() error { return writeUnixHandoff(s.config.StateRoot, handoff) }}
	active, err := unixHandoffActive(s.config.StateRoot, handoff)
	if err != nil {
		return err
	}
	manager, err := s.newManagerWithGate(active, gate)
	if err != nil {
		return err
	}
	s.managerMu.Lock()
	s.manager = manager
	s.managerMu.Unlock()
	if handoff.Started {
		err = manager.Recover(ctx)
	} else {
		handoff.Started = true
		if err = writeUnixHandoff(s.config.StateRoot, handoff); err != nil {
			return err
		}
		resolver := s.source.Resolve
		if handoff.Manual {
			resolver = s.source.ResolveManual
		}
		_, err = manager.Check(ctx, func(ctx context.Context) (workerupdate.Release, bool, error) {
			release, found, resolveErr := resolver(ctx)
			if resolveErr == nil && found && release.Version != handoff.Candidate {
				return workerupdate.Release{}, false, errors.New("signed eligible release changed before activation; retry update")
			}
			return release, found, resolveErr
		})
	}
	state, stateErr := manager.TransactionState()
	if stateErr != nil {
		return errors.Join(err, stateErr)
	}
	if state.Stage != "idle" {
		return errors.Join(err, errors.New("update recovery remains pending"))
	}
	if handoff.ParticipantsTouched {
		// A reboot after the journal became idle can interrupt the final local
		// restart. Reprove the committed/restored installation before retirement.
		timeout := handoff.RecoveryTimeout
		if timeout <= 0 {
			timeout = 2 * time.Minute
		}
		recoveryCtx, cancel := context.WithTimeout(ctx, timeout)
		readinessErr := gate.probe(recoveryCtx, state.ActiveVersion)
		if readinessErr != nil {
			readinessErr = gate.restart(recoveryCtx, state.ActiveVersion)
		}
		cancel()
		if readinessErr != nil {
			return readinessErr
		}
	}
	// No participant restart is needed when staging failed before cutover.
	// After a cutover, gate Active/Rollback has already proven exact processes.
	if err != nil {
		slog.Error("verified update activation did not complete; previous installation retained", "error", err)
	}
	return retireUnixHandoff(ctx, s.config.StateRoot, s.config.ActivationController)
}

func verifyUnixExecutable(path string, release workerupdate.Release) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() != release.Length {
		return workerupdate.ErrInvalidRelease
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err = io.Copy(digest, io.LimitReader(file, release.Length+1)); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != release.SHA256 {
		return workerupdate.ErrInvalidRelease
	}
	return nil
}

func removeUnixHandoff(root string) error {
	if err := os.Remove(unixHandoffPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncUnixDirectory(filepath.Join(root, "activation")); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(root, "activation", "pb")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncUnixDirectory(filepath.Join(root, "activation"))
}

// A cached or connected installation must regain readiness. Only an already
// empty installation whose sole failure is remote control-plane availability
// may preserve that same degraded state after local recovery.
func unixParticipantReady(probe UnixParticipantProbe, version string, baseline UnixParticipantProbe, requireInventory bool) error {
	if probe.UpdaterVersion != version {
		return fmt.Errorf("%w: ordinary updater version does not match %s", ErrParticipantReadiness, version)
	}
	if !baseline.Running {
		return nil
	}
	if !probe.Running || probe.Version != version {
		return fmt.Errorf("%w: enrolled daemon version does not match %s", ErrParticipantReadiness, version)
	}
	if probe.State == "ready" {
		return nil
	}
	if !requireInventory && baseline.State == "degraded" && baseline.Machines == 0 && baseline.ControlPlaneUnavailableOnly && probe.State == "degraded" && probe.Machines == 0 && probe.ControlPlaneUnavailableOnly {
		return nil
	}
	return fmt.Errorf("%w: enrolled daemon has not recovered its prior usable state", ErrParticipantReadiness)
}

type unixParticipantGate struct {
	inner        workerupdate.ActivationGate
	participants UnixParticipants
	controller   UnixActivationController
	handoff      *unixActivationHandoff
	persist      func() error
}

func (g *unixParticipantGate) Candidate(ctx context.Context, r workerupdate.GateRequest) error {
	return g.inner.Candidate(ctx, r)
}
func (g *unixParticipantGate) Drain(ctx context.Context, r workerupdate.GateRequest) error {
	probe, err := g.participants.Probe(ctx)
	if err != nil {
		return errors.Join(ErrParticipantReadiness, err)
	}
	if probe.Running {
		g.handoff.Baseline.Running = true
	}
	if probe.State == "ready" || probe.Machines > 0 {
		g.handoff.RequireInventory = true
	}
	if err = unixParticipantReady(probe, r.Previous.Version, g.handoff.Baseline, g.handoff.RequireInventory); err != nil {
		return err
	}
	if err = g.persist(); err != nil {
		return err
	}
	err = g.inner.Drain(ctx, r)
	var activeSessions *autoupdate.ActiveTerminalSessionsError
	if !errors.As(err, &activeSessions) {
		return err
	}
	g.handoff.ActiveTerminalBusy = true
	g.handoff.BusyRequiredVersion = activeSessions.RequiredVersion
	g.handoff.BusyNextCheckAt = time.Now().UTC().Add(autoupdate.DefaultRetryFloor)
	if persistErr := g.persist(); persistErr != nil {
		// Persistence is required to prevent the replacement updater from
		// immediately handing this same transaction back to the helper.
		return errors.Join(ErrParticipantReadiness, persistErr)
	}
	return activeSessions
}
func (g *unixParticipantGate) Commit(ctx context.Context, r workerupdate.GateRequest) error {
	return g.inner.Commit(ctx, r)
}
func (g *unixParticipantGate) Active(ctx context.Context, r workerupdate.GateRequest) error {
	// Start both observation paths together; neither extends the signed worker
	// stability deadline by spending another window on local participants.
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.inner.Active(watchCtx, r) }()
	err := g.restart(watchCtx, r.Candidate.Version)
	if err != nil {
		cancel()
		<-done
		return err
	}
	interval := r.Interval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case err = <-done:
			if err != nil {
				return err
			}
			return g.probe(ctx, r.Candidate.Version)
		case <-ticker.C:
			if err = g.probe(watchCtx, r.Candidate.Version); err != nil {
				cancel()
				<-done
				return err
			}
		case <-ctx.Done():
			cancel()
			<-done
			return errors.Join(ErrParticipantReadiness, ctx.Err())
		}
	}
}
func (g *unixParticipantGate) Rollback(ctx context.Context, r workerupdate.GateRequest) error {
	if !g.handoff.ParticipantsTouched {
		// Before Active, this gate has not restarted any participant. This also
		// covers a crash after inner Drain reports busy but before the handoff
		// can record that result. Restore updater ownership and the inner fence
		// without disturbing the running daemon or its terminal processes.
		if err := g.controller.RestartUpdater(ctx); err != nil {
			return errors.Join(ErrParticipantReadiness, err)
		}
		return g.inner.Rollback(ctx, r)
	}
	// Rollback requests name the failed release as Previous and the restored
	// release as Candidate, matching the worker activation direction.
	if err := g.restart(ctx, r.Candidate.Version); err != nil {
		return err
	}
	return g.inner.Rollback(ctx, r)
}
func (g *unixParticipantGate) restart(ctx context.Context, version string) error {
	g.handoff.ParticipantsTouched = true
	if err := g.persist(); err != nil {
		return err
	}
	if err := g.controller.RestartUpdater(ctx); err != nil {
		return errors.Join(ErrParticipantReadiness, err)
	}
	if g.handoff.Baseline.Running {
		if err := g.participants.Restart(ctx); err != nil {
			return errors.Join(ErrParticipantReadiness, err)
		}
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		if last = g.probe(ctx, version); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrParticipantReadiness, last, ctx.Err())
		case <-ticker.C:
		}
	}
}
func (g *unixParticipantGate) probe(ctx context.Context, version string) error {
	probe, err := g.participants.Probe(ctx)
	if err != nil {
		return errors.Join(ErrParticipantReadiness, err)
	}
	return unixParticipantReady(probe, version, g.handoff.Baseline, g.handoff.RequireInventory)
}

// LockUnixActivationForUninstall holds the same ownership lock through native
// job removal and installed-file deletion. An active update leaves uninstall
// untouched and returns an actionable retry, rather than killing recovery.
func LockUnixActivationForUninstall(root string) (io.Closer, error) {
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	lock, err := unixActivationLock(root)
	if errors.Is(err, ErrActivationPending) {
		return nil, fmt.Errorf("finish the pending update before uninstalling: %w", err)
	}
	if err != nil {
		return nil, err
	}
	if _, err := readUnixHandoff(root); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

func retireUnixHandoff(ctx context.Context, root string, controller UnixActivationController) error {
	if err := controller.Retire(ctx); err != nil {
		return err
	}
	return removeUnixHandoff(root)
}
