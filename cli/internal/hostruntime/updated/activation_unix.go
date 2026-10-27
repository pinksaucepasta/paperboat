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
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"golang.org/x/sys/unix"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

var ErrActivationPending = errors.New("verified update activation is owned by the persistent recovery helper")
var ErrParticipantReadiness = errors.New("updated installation readiness unavailable")

type UnixParticipantProbe struct {
	Version                     string   `json:"version"`
	UpdaterVersion              string   `json:"updater_version"`
	State                       string   `json:"state"`
	Machines                    uint32   `json:"machines"`
	Running                     bool     `json:"running"`
	ControlPlaneUnavailableOnly bool     `json:"control_plane_unavailable_only"`
	Health                      []string `json:"health,omitempty"` // the daemon's health item codes
}

type UnixParticipants interface {
	Probe(context.Context) (UnixParticipantProbe, error)
	Restart(context.Context) error
	// OpenConnections counts the SSH streams and access forwards the enrolled daemon carries. An
	// update replaces the daemon and ends them.
	OpenConnections(context.Context) (liveconnections.Counts, error)
}

// UnixActivationController operates fixed native jobs, never a release-selected
// command. Install registers the verified previous executable as a durable job.
type UnixActivationController interface {
	Install(context.Context, string, map[string]string) error
	Retire(context.Context) error
}

type unixActivationHandoff struct {
	Schema                  string               `json:"schema"`
	PriorOwnerRecorded      bool                 `json:"prior_owner_recorded"`
	OwnerProvenanceRecorded bool                 `json:"owner_provenance_recorded"`
	Previous                workerupdate.Release `json:"previous"`
	Candidate               string               `json:"candidate"`
	ApprovalID              string               `json:"approval_id"`
	Manual                  bool                 `json:"manual"`
	Force                   bool                 `json:"force,omitempty"`
	Started                 bool                 `json:"started"`
	Baseline                UnixParticipantProbe `json:"baseline"`
	RequireInventory        bool                 `json:"require_inventory"`
	RecoveryTimeout         time.Duration        `json:"recovery_timeout"`
	ParticipantsTouched     bool                 `json:"participants_touched"`
	ActiveTerminalBusy      bool                 `json:"active_terminal_busy,omitempty"`
	BusyRequiredVersion     string               `json:"busy_required_version,omitempty"`
	BusyNextCheckAt         time.Time            `json:"busy_next_check_at,omitempty,omitzero"`
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
	if decoder.Decode(&value) != nil || decoder.Decode(&extra) != io.EOF || value.Schema != unixHandoffSchema || !approvalIDPattern.MatchString(value.ApprovalID) || value.Previous.Version == "" || value.Previous.Length <= 0 || len(value.Previous.SHA256) != 64 || value.Candidate == "" || value.ActiveTerminalBusy != (value.BusyRequiredVersion != "" && !value.BusyNextCheckAt.IsZero()) {
		return nil, ErrInvalidConfig
	}
	return &value, nil
}

// UnixActivationActive lets a restarted ordinary updater initialize without
// refreshing shared TUF state while the recovery helper owns the transaction.
func UnixActivationActive(root, binary string, loadInstalled func() (installsource.Source, error)) (workerupdate.Release, bool, error) {
	handoff, err := readUnixHandoff(root)
	if err != nil || handoff == nil {
		return workerupdate.Release{}, false, err
	}
	active, err := unixHandoffActive(root, handoff, binary, loadInstalled)
	return active, true, err
}

func unixHandoffActive(root string, handoff *unixActivationHandoff, binary string, loadInstalled func() (installsource.Source, error)) (workerupdate.Release, error) {
	journal, err := updateflow.Load(filepath.Join(root, "transaction.json"))
	if errors.Is(err, os.ErrNotExist) {
		return handoff.Previous, nil
	}
	if err != nil {
		return workerupdate.Release{}, err
	}
	if journal.Stage == updateflow.StageIdle {
		if loadInstalled != nil {
			source, err := loadInstalled()
			if err != nil {
				return workerupdate.Release{}, err
			}
			return workerupdate.InstalledBaseline(source, binary)
		}
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

func (s *Service) queueActivation(ctx context.Context, approvalID string) (workerupdate.Result, error) {
	return s.queueActivationWithApproval(ctx, approvalID, true, false)
}

// authorizeUnixHandoffForce runs under the activation flock. Once the helper
// starts, its in-memory intent is immutable; retry cannot claim a new force grant.
func authorizeUnixHandoffForce(root string, handoff *unixActivationHandoff, approvalID string, force bool) error {
	if handoff.ApprovalID != approvalID {
		return ErrCandidateChanged
	}
	if !force || handoff.Force {
		return nil
	}
	if handoff.Started || handoff.ParticipantsTouched {
		return ErrActivationPending
	}
	handoff.Force, handoff.Manual = true, true
	return writeUnixHandoff(root, handoff)
}

func (s *Service) queueActivationWithApproval(ctx context.Context, approvalID string, manual, force bool) (workerupdate.Result, error) {
	if err := s.requireCommittedSource(); err != nil {
		return workerupdate.Result{}, err
	}
	force = manual && force
	if !approvalIDPattern.MatchString(approvalID) {
		return workerupdate.Result{}, ErrApprovalRequired
	}
	result := workerupdate.Result{Version: s.currentManager().ActiveVersion()}
	lock, err := unixActivationLock(s.config.StateRoot)
	if errors.Is(err, ErrActivationPending) {
		if force {
			return result, ErrActivationPending
		}
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if pending, err := nativeInstallPending(s.config.StateRoot); err != nil || pending {
		return result, errors.Join(ErrActivationPending, err)
	}
	handoff, err := readUnixHandoff(s.config.StateRoot)
	if err != nil {
		return result, err
	}
	if handoff != nil {
		if err := authorizeUnixHandoffForce(s.config.StateRoot, handoff, approvalID, force); err != nil {
			return result, err
		}
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
	resolve := s.source.ResolveManual
	if !manual {
		resolve = s.source.Resolve
	}
	release, found, err := resolve(ctx)
	if err != nil || !found {
		return result, err
	}
	prepared, err := manager.PreparedCandidate()
	if err != nil {
		return result, err
	}
	current, err := workerupdate.PreparedCandidateForRelease(release)
	if err != nil {
		return result, err
	}
	if prepared.ID == "" {
		return result, ErrApprovalRequired
	}
	if prepared.ID != approvalID || current.ID != prepared.ID {
		return result, ErrCandidateChanged
	}
	owner, err := hostdproto.NewClient(s.config.SocketPath, s.config.Token, 35*time.Second)
	if err != nil {
		return result, err
	}
	if err := planOwnerMaintenance(ctx, s.config.StateRoot, owner, release, force); err != nil {
		return result, err
	}
	state, err := manager.TransactionState()
	if err != nil {
		return result, err
	}
	if state.Quarantined && state.CandidateVersion == release.Version {
		return result, workerupdate.ErrQuarantined
	}
	if s.config.Active.LocalSource == nil {
		if err = s.source.AuthorizeRecovery(ctx, s.config.Active.Version, s.config.Active.Platform, s.config.Active.Architecture); err != nil {
			return result, err
		}
	} else if err = s.config.Active.LocalSource.Verify(s.config.Binary); err != nil {
		return result, err
	}
	baseline, err := s.config.Participants.Probe(ctx)
	if err != nil {
		return result, errors.Join(ErrParticipantReadiness, err)
	}
	if err = unixParticipantReady(baseline, s.config.Active.Version, baseline, baseline.Machines > 0 || baseline.State == "ready"); err != nil {
		return result, err
	}
	if err = s.refuseWhileConnectionsAreOpen(ctx, release.Version, force); err != nil {
		return result, err
	}
	if err = manager.Approve(ctx, approvalID); err != nil {
		return result, err
	}
	priorOwnerRecorded := false
	if s.config.PriorOwnerRecorded != nil {
		priorOwnerRecorded, err = s.config.PriorOwnerRecorded(ctx, s.config.Active)
		if err != nil {
			return result, err
		}
	}
	handoff = &unixActivationHandoff{Schema: unixHandoffSchema, PriorOwnerRecorded: priorOwnerRecorded, OwnerProvenanceRecorded: s.config.PriorOwnerRecorded != nil, ApprovalID: approvalID, Previous: s.config.Active, Candidate: release.Version, Manual: manual, Force: force, RecoveryTimeout: release.RollbackTimeout, Baseline: baseline, RequireInventory: baseline.Machines > 0 || baseline.State == "ready"}
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
	var active workerupdate.Release
	var err error
	if s.config.LoadInstalledSource != nil {
		var pending bool
		active, pending, err = UnixActivationActive(s.config.StateRoot, s.config.Binary, s.config.LoadInstalledSource)
		if err == nil && !pending {
			var source installsource.Source
			source, err = s.config.LoadInstalledSource()
			if err == nil {
				active, err = workerupdate.InstalledBaseline(source, s.config.Binary)
			}
		}
	} else {
		active, err = workerupdate.ActiveReleaseFromJournal(filepath.Join(s.config.StateRoot, "transaction.json"), buildinfo.Version)
		if errors.Is(err, os.ErrNotExist) {
			active, err = s.config.Active, nil
		}
	}
	if err != nil {
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
func (s *Service) RunActivationHelper(ctx context.Context) (resultErr error) {
	lock, err := unixActivationLock(s.config.StateRoot)
	if err != nil {
		return err
	}
	lockHeld := true
	releaseLock := func() error {
		if !lockHeld {
			return nil
		}
		lockHeld = false
		return lock.Close()
	}
	defer func() { resultErr = errors.Join(resultErr, releaseLock()) }()
	handoff, err := readUnixHandoff(s.config.StateRoot)
	if err != nil {
		return err
	}
	if handoff == nil {
		if err := releaseLock(); err != nil {
			return err
		}
		return s.config.ActivationController.Retire(ctx)
	}
	var activationErr error
	diagnosticWritten := false
	defer func() {
		if !diagnosticWritten {
			writeActivationFailure(os.Stderr, errors.Join(activationErr, resultErr), handoff.Previous.Version, handoff.Candidate)
		}
	}()
	preferences, err := machineUpdateSettings(s.config.StateRoot, s.config.AutomaticUpdates, nil)
	if err != nil {
		return err
	}
	if !preferences.Enabled && !handoff.Manual && !handoff.Started {
		if err := releaseLock(); err != nil {
			return err
		}
		return retireUnixHandoff(ctx, s.config.StateRoot, s.config.ActivationController)
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
	readiness := &unixParticipantReadiness{participants: s.config.Participants, handoff: handoff}
	active, err := unixHandoffActive(s.config.StateRoot, handoff, s.config.Binary, s.config.LoadInstalledSource)
	if err != nil {
		return err
	}
	manager, err := s.newManagerForActivation(active, handoff.Manual, handoff.Force)
	if err != nil {
		return err
	}
	s.managerMu.Lock()
	s.manager = manager
	s.managerMu.Unlock()
	stateBefore, stateBeforeErr := manager.TransactionState()
	if stateBeforeErr != nil {
		return stateBeforeErr
	}
	if handoff.Started && stateBefore.Stage != updateflow.StageAwaitingApproval {
		err = manager.Recover(ctx)
	} else {
		handoff.Started = true
		if err = writeUnixHandoff(s.config.StateRoot, handoff); err != nil {
			return err
		}
		release, resolveErr := s.resolveUnixPreparedRelease(ctx, handoff, manager)
		if resolveErr != nil {
			// An earlier attempt that was rolled back consumed the approved payload, so this
			// start can never activate it. End the attempt (journal reset to idle, installation
			// left on the previous version) instead of failing the same way on every restart.
			if !errors.Is(resolveErr, workerupdate.ErrPreparedCandidate) {
				return resolveErr
			}
			abandoned, abandonErr := manager.AbandonUnavailableCandidate(ctx)
			if abandonErr != nil || !abandoned {
				return errors.Join(resolveErr, abandonErr)
			}
			err = resolveErr
		} else {
			if s.config.ApplyPrepared != nil {
				// Native replacement may stop participants before the signed gate
				// observes Active. Persist that ownership before entering it.
				handoff.ParticipantsTouched = true
				if err = writeUnixHandoff(s.config.StateRoot, handoff); err != nil {
					return err
				}
			}
			_, err = manager.Activate(ctx, release)
		}
	}
	activationErr = err
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
		readinessErr := readiness.probe(recoveryCtx, state.ActiveVersion)
		cancel()
		if readinessErr != nil {
			return readinessErr
		}
	}
	// No participant restart is needed when staging failed before cutover.
	// After replacement, native validation has already proven exact processes.
	if closeErr := releaseLock(); closeErr != nil {
		resultErr = errors.Join(resultErr, closeErr)
	}
	if diagnosticErr := errors.Join(activationErr, resultErr); diagnosticErr != nil && !diagnosticWritten {
		// Retirement can stop this native helper. Emit the bounded cause before
		// asking its supervisor to retire it, including any lock-release failure.
		writeActivationFailure(os.Stderr, diagnosticErr, handoff.Previous.Version, handoff.Candidate)
		diagnosticWritten = true
	}
	resultErr = errors.Join(resultErr, retireUnixHandoff(ctx, s.config.StateRoot, s.config.ActivationController))
	return resultErr
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
		return participantFailure("updater_version", ErrParticipantReadiness)
	}
	if !baseline.Running {
		return nil
	}
	if !probe.Running {
		return participantFailure("daemon_stopped", ErrParticipantReadiness)
	}
	if probe.Version != version {
		return participantFailure("daemon_version", ErrParticipantReadiness)
	}
	if probe.State == "ready" {
		return nil
	}
	// An installation that was already unhealthy before the update, for a reason other than the
	// control plane being unreachable (a broken local access setup, say), can only be repaired by
	// updating it. The updater must work when the app is broken: the daemon has to come back
	// running the right version, not ready, or the fix could never reach the machine.
	if baseline.State != "ready" && !baseline.ControlPlaneUnavailableOnly {
		return nil
	}
	// A control plane that cannot be reached, or that refuses this version (the very case an update
	// repairs), must not make the update impossible: the daemon may come back degraded for that same
	// reason as long as it kept the machines it had cached.
	if baseline.State == "degraded" && baseline.ControlPlaneUnavailableOnly && probe.State == "degraded" && probe.ControlPlaneUnavailableOnly && probe.Machines >= baseline.Machines {
		return nil
	}
	return participantFailure("daemon_state", ErrParticipantReadiness)
}

func participantFailure(reason string, cause error) error {
	return &activationFailure{phase: "participant", reason: reason, cause: cause}
}

// The helper runs under native service supervision. This single bounded stderr
// record remains available even when the privacy-filtered diagnostic store
// cannot be written. No wrapped error or arbitrary probe value is formatted.
func writeActivationFailure(writer io.Writer, err error, previous, candidate string) {
	if err == nil {
		return
	}
	phase, reason := "activation", "failed"
	var failure *activationFailure
	if errors.As(err, &failure) {
		switch failure.phase + "/" + failure.reason {
		case "health/owner_identity", "health/heartbeat_missing", "health/heartbeat_stale", "health/endpoint_invalid", "health/request_invalid", "health/transport", "health/http_redirect", "health/http_status", "health/http_body", "health/http_not_live", "participant/updater_version", "participant/daemon_stopped", "participant/daemon_version", "participant/daemon_state", "participant/probe", "participant/restart_daemon":
			phase, reason = failure.phase, failure.reason
		}
	}
	classification := "failure"
	var timeout interface{ Timeout() bool }
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		classification = "timeout"
	} else if errors.Is(err, context.Canceled) {
		classification = "canceled"
	}
	validVersion := func(value string) string {
		if len(value) <= 64 && exactReleasePattern.MatchString(value) {
			return value
		}
		return "unknown"
	}
	chain, cause, sentinels := activationErrorChain(err)
	_, _ = fmt.Fprintf(writer, "paperboat-update-failure phase=%s reason=%s previous=%s candidate=%s classification=%s chain=%s io=%s sentinels=%s\n", phase, reason, validVersion(previous), validVersion(candidate), classification, chain, cause, sentinels)
}

var (
	errorTypeName = regexp.MustCompile(`^[*A-Za-z0-9_./]{1,80}$`)
	fileOperation = regexp.MustCompile(`^[a-z]{1,16}$`)
	errnoNameRE   = regexp.MustCompile(`^E[A-Z0-9]{1,16}$`)
)

// activationErrorChain names the Go types along a bounded walk of the error tree, never their
// messages or values, so a failure that carries no finite phase still shows which step produced it
// (for example *exec.ExitError or hostinstall.errLifecycle...) without leaking paths or credentials.
// The second result names the first file operation and system error in the tree (for example
// "lstat:ENOENT"), or "none"; the path itself is never included.
func activationErrorChain(err error) (string, string, string) {
	queue := []error{err}
	seen := map[string]bool{}
	var names []string
	operation, errno := "", ""
	var matched []string
	for visited := 0; len(queue) > 0 && visited < 24 && len(names) < 8; visited++ {
		current := queue[0]
		queue = queue[1:]
		if current == nil {
			continue
		}
		if name := fmt.Sprintf("%T", current); errorTypeName.MatchString(name) && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		if named, ok := current.(interface{ GateOperationName() string }); ok && fileOperation.MatchString(named.GateOperationName()) {
			if label := "GateOperation_" + named.GateOperationName(); !slices.Contains(matched, label) {
				matched = append(matched, label)
			}
		}
		for _, known := range knownActivationSentinels {
			if reflect.TypeOf(current).Comparable() && current == known.err && !slices.Contains(matched, known.name) {
				matched = append(matched, known.name)
			}
		}
		switch typed := current.(type) {
		case *fs.PathError:
			if operation == "" && fileOperation.MatchString(typed.Op) {
				operation = typed.Op
			}
		case syscall.Errno:
			if errno == "" {
				errno = unix.ErrnoName(typed)
				if !errnoNameRE.MatchString(errno) {
					errno = "errno" + strconv.Itoa(int(typed))
				}
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() error }:
			queue = append(queue, wrapped.Unwrap())
		case interface{ Unwrap() []error }:
			queue = append(queue, wrapped.Unwrap()...)
		}
	}
	cause := "none"
	if operation != "" || errno != "" {
		cause = operation + ":" + errno
	}
	sentinels := "none"
	if len(matched) > 0 {
		sentinels = strings.Join(matched, ",")
	}
	if len(names) == 0 {
		return "none", cause, sentinels
	}
	return strings.Join(names, ","), cause, sentinels
}

// Sentinel errors carry fixed text, so naming the ones present in a failure says which step
// failed even though every one of them has the same Go type.
var knownActivationSentinels = []struct {
	name string
	err  error
}{
	{"ActivationGate", workerupdate.ErrActivationGate},
	{"Blocked", workerupdate.ErrBlocked},
	{"ApprovalRequired", workerupdate.ErrApprovalRequired},
	{"PreparedCandidate", workerupdate.ErrPreparedCandidate},
	{"InvalidRelease", workerupdate.ErrInvalidRelease},
	{"Quarantined", workerupdate.ErrQuarantined},
	{"HostdActivationProvider", workerupdate.ErrHostdActivationProvider},
	{"LifecycleUncertain", service.ErrLifecycleUncertain},
	{"LifecycleNotReady", service.ErrLifecycleNotReady},
	{"ParticipantReadiness", ErrParticipantReadiness},
	{"InstallationPending", ErrInstallationPending},
}

type unixParticipantReadiness struct {
	participants UnixParticipants
	handoff      *unixActivationHandoff
}

// waitReady polls the restarted daemon until it is ready for version. For a candidate, a daemon
// degraded for a local reason fails at once; one only waiting for the control plane is waited for, and
// at the deadline the error is ErrParticipantControlPlaneTimeout. Recovery to the previous version
// keeps polling on any failure.
func (g *unixParticipantReadiness) waitReady(ctx context.Context, version string, candidate bool) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var last error
	wait := ParticipantNotReady
	for {
		probe, err := g.participants.Probe(ctx)
		if err != nil {
			wait, last = ParticipantNotReady, participantFailure("probe", errors.Join(ErrParticipantReadiness, err))
		} else if wait, last = ClassifyParticipant(probe, version, g.handoff.Baseline, g.handoff.RequireInventory); wait == ParticipantReadyNow {
			return nil
		} else if candidate && wait == ParticipantDegradedLocally {
			return errors.Join(ErrParticipantReadiness, last)
		}
		select {
		case <-ctx.Done():
			if wait == ParticipantWaitingForControlPlane {
				return errors.Join(ErrParticipantReadiness, ErrParticipantControlPlaneTimeout, last, ctx.Err())
			}
			return errors.Join(ErrParticipantReadiness, last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (g *unixParticipantReadiness) probe(ctx context.Context, version string) error {
	probe, err := g.participants.Probe(ctx)
	if err != nil {
		return participantFailure("probe", errors.Join(ErrParticipantReadiness, err))
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

// A busy manual activation retains only its original version's cohort override.
// Every retry resolves fresh signed metadata and still enforces revocation,
// deferral, expiry, hashes and the current deployment policy.
func resolveQueuedRelease(ctx context.Context, manual bool, journal updateflow.Journal, automatic, explicit workerupdate.Resolver) (workerupdate.Release, bool, bool, error) {
	deferred := journal.BlockedReason == autoupdate.BlockedActiveTerminalSessions && journal.DeferredManual
	effectiveManual := manual || deferred
	resolver := automatic
	if effectiveManual {
		resolver = explicit
	}
	release, found, err := resolver(ctx)
	if err == nil && deferred && !manual && (!found || release.Version != journal.RequiredVersion) {
		return workerupdate.Release{}, false, effectiveManual, errors.New("deferred manual update is no longer the current eligible signed release; run pb update to choose a new release")
	}
	return release, found, effectiveManual, err
}

// UnixLegacyRollback recognizes only the exact protected preactivation source
// whose descriptor absence was recorded before native target replacement.
func UnixLegacyRollback(root string, release workerupdate.Release) (bool, error) {
	handoff, err := readUnixHandoff(root)
	if err != nil {
		return false, err
	}
	if handoff == nil || !handoff.OwnerProvenanceRecorded || handoff.PriorOwnerRecorded {
		return false, nil
	}
	previous := handoff.Previous
	return release.Version == previous.Version && release.SHA256 == previous.SHA256 && release.Length == previous.Length && release.Platform == previous.Platform && release.Architecture == previous.Architecture, nil
}

// applyPrepared lets the native transaction validate the per-user routing
// daemon as well as hostd/updater. Terminal owners retain independent lifetimes.
func (s *Service) applyPrepared(ctx context.Context, candidate workerupdate.PreparedActivation) (installsource.Source, error) {
	handoff, err := readUnixHandoff(s.config.StateRoot)
	if err != nil {
		return installsource.Source{}, err
	}
	if handoff == nil || candidate.Validate == nil || s.config.LoadInstalledSource == nil {
		return installsource.Source{}, ErrInvalidConfig
	}
	prepare := candidate.Prepare
	if prepare == nil {
		return installsource.Source{}, ErrInvalidConfig
	}
	candidate.Prepare = func(prepareCtx context.Context) error {
		probe, err := s.config.Participants.Probe(prepareCtx)
		if err != nil {
			return participantFailure("probe", errors.Join(ErrParticipantReadiness, err))
		}
		if probe.Running {
			handoff.Baseline.Running = true
		}
		if probe.State == "ready" || probe.Machines > 0 {
			handoff.RequireInventory = true
		}
		if err := unixParticipantReady(probe, handoff.Previous.Version, handoff.Baseline, handoff.RequireInventory); err != nil {
			return err
		}
		if err := writeUnixHandoff(s.config.StateRoot, handoff); err != nil {
			return err
		}
		return prepare(prepareCtx)
	}
	validate := candidate.Validate
	readiness := &unixParticipantReadiness{participants: s.config.Participants, handoff: handoff}
	timeout := handoff.RecoveryTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	routingTouched := false
	candidate.Validate = func(validateCtx context.Context) error {
		if handoff.Baseline.Running {
			routingTouched = true
			// A retry after Source commit can already have the exact routing daemon.
			probe, probeErr := s.config.Participants.Probe(validateCtx)
			if probeErr != nil || unixParticipantReady(probe, candidate.Source.Version, handoff.Baseline, handoff.RequireInventory) != nil {
				if err := s.config.Participants.Restart(validateCtx); err != nil {
					return participantFailure("restart_daemon", errors.Join(ErrParticipantReadiness, err))
				}
			}
		}
		readyCtx, cancel := context.WithTimeout(validateCtx, timeout)
		readyErr := readiness.waitReady(readyCtx, candidate.Source.Version, true)
		cancel()
		if readyErr != nil {
			return readyErr
		}
		if err := validate(validateCtx); err != nil {
			return err
		}
		probe, err := s.config.Participants.Probe(validateCtx)
		if err != nil {
			return participantFailure("probe", errors.Join(ErrParticipantReadiness, err))
		}
		return unixParticipantReady(probe, candidate.Source.Version, handoff.Baseline, handoff.RequireInventory)
	}
	committed, err := s.config.ApplyPrepared(ctx, candidate)
	if err == nil {
		return committed, nil
	}
	if errors.Is(err, service.ErrLifecycleUncertain) {
		return installsource.Source{}, errors.Join(err, workerupdate.ErrBlocked)
	}
	// The native owner must have restored and re-authorized the exact baseline
	// before a user daemon can restart against it. Pending recovery fails closed.
	source, sourceErr := s.config.LoadInstalledSource()
	if sourceErr != nil || source.Version != handoff.Previous.Version || source.SHA256 != handoff.Previous.SHA256 || source.Length != handoff.Previous.Length {
		return installsource.Source{}, errors.Join(err, sourceErr, workerupdate.ErrBlocked)
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if verifyErr := source.Verify(s.config.Binary); verifyErr != nil {
		return installsource.Source{}, errors.Join(err, verifyErr, workerupdate.ErrBlocked)
	}
	if !routingTouched {
		return source, err
	}
	restartErr := s.config.Participants.Restart(recoveryCtx)
	if restartErr != nil {
		return installsource.Source{}, errors.Join(err, participantFailure("restart_daemon", errors.Join(ErrParticipantReadiness, restartErr)))
	}
	if readinessErr := readiness.waitReady(recoveryCtx, source.Version, false); readinessErr != nil {
		return installsource.Source{}, errors.Join(err, readinessErr)
	}
	return source, err
}

// recoverPrepared runs the native recovery of an installation interrupted before its commit. When
// that restored the previous version, the routing daemon restarts on it, as after a rolled-back
// attempt, before the attempt ends.
func (s *Service) recoverPrepared(ctx context.Context) (installsource.Source, error) {
	handoff, err := readUnixHandoff(s.config.StateRoot)
	if err != nil {
		return installsource.Source{}, err
	}
	if handoff == nil {
		return installsource.Source{}, ErrInvalidConfig
	}
	committed, err := s.config.RecoverPrepared(ctx)
	if err != nil {
		return installsource.Source{}, err
	}
	previous := handoff.Previous
	if committed.Version != previous.Version || committed.SHA256 != previous.SHA256 || committed.Length != previous.Length {
		return committed, nil
	}
	timeout := handoff.RecoveryTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if handoff.Baseline.Running {
		if err := s.config.Participants.Restart(recoveryCtx); err != nil {
			return installsource.Source{}, participantFailure("restart_daemon", errors.Join(ErrParticipantReadiness, err))
		}
	}
	readiness := &unixParticipantReadiness{participants: s.config.Participants, handoff: handoff}
	if err := readiness.waitReady(recoveryCtx, committed.Version, false); err != nil {
		return installsource.Source{}, err
	}
	return committed, nil
}

// resolveUnixPreparedRelease keeps ordinary queued approval pinned to the current
// candidate; only protected native progress permits resuming its original bytes.
func (s *Service) resolveUnixPreparedRelease(ctx context.Context, handoff *unixActivationHandoff, manager *workerupdate.Manager) (workerupdate.Release, error) {
	var original workerupdate.Release
	var nativeStarted bool
	if s.config.ApplyPrepared != nil && s.config.NativeRecoveryStarted != nil {
		signed, payload, err := manager.PreparedRelease(ctx)
		if err != nil {
			return workerupdate.Release{}, err
		}
		original = signed
		candidate, err := workerupdate.PreparedCandidateForRelease(signed)
		if err != nil {
			return workerupdate.Release{}, err
		}
		if candidate.ID != handoff.ApprovalID || signed.Version != handoff.Candidate {
			return workerupdate.Release{}, ErrCandidateChanged
		}
		nativeStarted, err = s.config.NativeRecoveryStarted(ctx, handoff.Previous, payload)
		if err != nil {
			return workerupdate.Release{}, err
		}
	}
	resolve := s.source.ResolveManual
	if !handoff.Manual {
		resolve = s.source.Resolve
	}
	return resolveUnixActivationRelease(ctx, handoff, original, nativeStarted, resolve, s.source.AuthorizeRecovery)
}

// Native progress fixes the original approval; ordinary queued work still uses
// current eligibility. Both paths retain exact approval identity.
func resolveUnixActivationRelease(ctx context.Context, handoff *unixActivationHandoff, original workerupdate.Release, nativeStarted bool, resolve func(context.Context) (workerupdate.Release, bool, error), authorize func(context.Context, string, string, string) error) (workerupdate.Release, error) {
	release := original
	if nativeStarted {
		if err := authorize(ctx, release.Version, release.Platform, release.Architecture); err != nil {
			return workerupdate.Release{}, err
		}
		previous := handoff.Previous
		if previous.LocalSource == nil || previous.LocalSource.Distribution == installsource.Official {
			if err := authorize(ctx, previous.Version, previous.Platform, previous.Architecture); err != nil {
				return workerupdate.Release{}, err
			}
		}
	} else {
		var found bool
		var err error
		release, found, err = resolve(ctx)
		if err != nil {
			return workerupdate.Release{}, err
		}
		if !found {
			return workerupdate.Release{}, ErrCandidateChanged
		}
	}
	candidate, err := workerupdate.PreparedCandidateForRelease(release)
	if err != nil {
		return workerupdate.Release{}, err
	}
	if candidate.ID != handoff.ApprovalID || release.Version != handoff.Candidate {
		return workerupdate.Release{}, ErrCandidateChanged
	}
	return release, nil
}

// ParticipantReady reports whether the enrolled user's daemon (and the updater) came back usable on
// version, given how it was before the change. A daemon that was already unhealthy for a local reason
// only has to run the right version; see unixParticipantReady.
func ParticipantReady(probe UnixParticipantProbe, version string, baseline UnixParticipantProbe, requireInventory bool) error {
	return unixParticipantReady(probe, version, baseline, requireInventory)
}

// ParticipantWait is what one probe of a restarted daemon means for an update that waits for it.
type ParticipantWait int

const (
	// ParticipantNotReady: not running the version yet, still starting, or not probed; keep waiting.
	ParticipantNotReady ParticipantWait = iota
	// ParticipantReadyNow: ready by the readiness rule (unixParticipantReady).
	ParticipantReadyNow
	// ParticipantWaitingForControlPlane: the new version runs but its only problem is that the
	// control plane has not answered yet; keep waiting, never commit on it.
	ParticipantWaitingForControlPlane
	// ParticipantDegradedLocally: the new version runs and is degraded for another reason.
	ParticipantDegradedLocally
)

// ErrParticipantControlPlaneTimeout reports an update whose new daemon was still waiting for the
// Paperboat server at the deadline: the previous version is restored and the update can be retried.
var ErrParticipantControlPlaneTimeout = errors.New("the Paperboat server did not answer the new version's daemon before the update's deadline, so the previous version was restored; retry the update when the server is reachable")

// ClassifyParticipant applies the readiness rule to one probe; supplied and signed updates share it.
// The error is the readiness failure for any result but ParticipantReadyNow.
func ClassifyParticipant(probe UnixParticipantProbe, version string, baseline UnixParticipantProbe, requireInventory bool) (ParticipantWait, error) {
	err := unixParticipantReady(probe, version, baseline, requireInventory)
	switch {
	case err == nil:
		return ParticipantReadyNow, nil
	case probe.Running && probe.Version == version && probe.State == "degraded" && probe.ControlPlaneUnavailableOnly:
		return ParticipantWaitingForControlPlane, err
	case probe.Running && probe.Version == version && probe.State == "degraded":
		return ParticipantDegradedLocally, err
	default:
		return ParticipantNotReady, err
	}
}
