package updated

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

const windowsActivationJournalSchema = "paperboat.windows-activation/v1"
const maxWindowsComponentSize int64 = 256 << 20

type windowsActivationStage string

const (
	windowsActivationStaged              windowsActivationStage = "staged"
	windowsActivationCandidateValidating windowsActivationStage = "candidate_validating"
	windowsActivationCandidateReady      windowsActivationStage = "candidate_ready"
	windowsActivationDraining            windowsActivationStage = "draining"
	windowsActivationSwitching           windowsActivationStage = "switching"
	windowsActivationServicesLive        windowsActivationStage = "services_live"
	windowsActivationCommitted           windowsActivationStage = "committed"
	windowsActivationCommitReady         windowsActivationStage = "commit_ready"
	windowsActivationRollingBack         windowsActivationStage = "rolling_back"
	windowsActivationRollbackReady       windowsActivationStage = "rollback_ready"
	windowsActivationRolledBack          windowsActivationStage = "rolled_back"
	windowsActivationBusyReady           windowsActivationStage = "busy_ready"
)

type windowsActivationComponent struct {
	Path, SHA256 string
	Length       int64
}

type windowsServiceTarget struct {
	Executable string   `json:"executable"`
	Arguments  []string `json:"arguments"`
	WasRunning bool     `json:"was_running"`
}

type windowsActivationJournal struct {
	ManualMode                                                    string `json:",omitempty"`
	Schema, TransactionID, PreviousVersion, Version, Architecture string
	Stage                                                         windowsActivationStage
	Runtime, CLI, Hostd, Updater, PreviousBinary                  windowsActivationComponent
	OldHostd, NewHostd, OldUpdater, NewUpdater, OldSSH, NewSSH    windowsServiceTarget
	PreviousCLIRecord, NewCLIRecord                               string
	LocalDaemonWasRunning                                         bool
	PreDrainRollback                                              bool
	BlockedReason                                                 string    `json:",omitempty"`
	BlockedRetryAt                                                time.Time `json:",omitzero"`
	Failure                                                       string
	ManifestSHA256                                                string
	CanaryPath                                                    string
	CanaryStatus                                                  int
	CanarySamples                                                 uint16
	CanaryTimeout, DrainTimeout, StabilityWindow                  time.Duration
	StabilityInterval, RollbackTimeout                            time.Duration
	HostdAPIMin, HostdAPIMax, RuntimeAPIMin, RuntimeAPIMax        uint16
}

var errInvalidWindowsActivation = errors.New("invalid Windows activation transaction")

// windowsActivationBackend is deliberately narrow so the crash choreography
// has deterministic tests without pretending a macOS filesystem models SCM.
type windowsActivationBackend interface {
	AuthorizeRecovery(context.Context, windowsActivationJournal) error
	WriteJournal(windowsActivationJournal) error
	ProbeCandidate(context.Context, windowsActivationJournal) error
	StopCandidate(context.Context, windowsActivationJournal) error
	StopServices(context.Context, bool) error
	ActivateBinary(context.Context, windowsActivationJournal) error
	RestoreBinary(context.Context, windowsActivationJournal) error
	SetServiceTargets(context.Context, windowsServiceTarget, windowsServiceTarget, windowsServiceTarget) error
	StartServices(context.Context, bool, bool, bool, bool) error
	VerifyHealth(context.Context, windowsActivationJournal) error
	Drain(context.Context, windowsActivationJournal) error
	VerifyRollback(context.Context, windowsActivationJournal) error
	RollbackDrain(context.Context, windowsActivationJournal) error
	CommitCLI(context.Context, windowsActivationJournal) error
	CommitGate(context.Context, windowsActivationJournal) error
	Quarantine(context.Context, windowsActivationJournal) error
	FinalizeServices(context.Context, windowsActivationJournal) error
}

func executeWindowsActivation(ctx context.Context, backend windowsActivationBackend, journal windowsActivationJournal) (result windowsActivationJournal, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if backend == nil || !validWindowsActivationJournal(journal) {
		return journal, errInvalidWindowsActivation
	}
	if journal.Stage == windowsActivationCommitted || journal.Stage == windowsActivationRolledBack {
		if journal.Stage == windowsActivationCommitted {
			return completeWindowsCommit(ctx, backend, journal)
		}
		if err := backend.AuthorizeRecovery(ctx, journal); err != nil {
			return journal, err
		}
		return journal, backend.WriteJournal(journal)
	}
	if journal.Stage == windowsActivationCommitReady {
		return completeWindowsCommit(ctx, backend, journal)
	}
	// A candidate can fail before the durable drain boundary. Persisting that
	// fact keeps crash recovery from asking the rollback gate to verify a drain
	// that never happened. Post-drain and legacy ambiguous journals retain the
	// strict rollback-gate path below.
	if journal.BlockedReason == autoupdate.BlockedActiveTerminalSessions {
		return compensateWindowsBusy(ctx, backend, journal)
	}
	if journal.PreDrainRollback {
		return rollbackWindowsCandidate(ctx, backend, journal, errors.New("interrupted pre-drain rollback recovered"))
	}
	if journal.Stage == windowsActivationRollbackReady {
		return completeWindowsRollback(ctx, backend, journal, errors.New("interrupted rollback recovered"))
	}
	// Candidate validation happens before the old route is drained. If the
	// updater is interrupted at either candidate stage, the old service and
	// route remain authoritative; only the staged candidate needs to be
	// stopped and quarantined.
	if journal.Stage == windowsActivationCandidateValidating || journal.Stage == windowsActivationCandidateReady {
		return rollbackWindowsCandidate(ctx, backend, journal, errors.New("interrupted candidate validation recovered"))
	}
	// Draining is durably recorded before the RPC and before any canonical
	// process is stopped. Reconcile its exact admission fence without turning
	// a lost busy response into termination of the user's live terminal.
	if journal.Stage == windowsActivationDraining {
		return rollbackWindowsDrain(ctx, backend, journal, errors.New("interrupted drain recovered before cutover"))
	}
	// Once a previous activator may have changed SCM, recovery always restores
	// the old exact commands first. It never guesses which candidate process
	// survived a power loss.
	if journal.Stage != windowsActivationStaged {
		return rollbackWindowsActivation(ctx, backend, journal, errors.New("interrupted activation recovered"))
	}
	// Refuse cutover before touching services when the trusted previous
	// installation is no longer permitted by signed recovery policy.
	if err := backend.AuthorizeRecovery(ctx, journal); err != nil {
		return journal, err
	}
	journal.Stage = windowsActivationCandidateValidating
	if err = backend.WriteJournal(journal); err != nil {
		return journal, err
	}
	if err = backend.ProbeCandidate(ctx, journal); err != nil {
		return rollbackWindowsCandidate(ctx, backend, journal, err)
	}
	journal.Stage = windowsActivationCandidateReady
	if err = backend.WriteJournal(journal); err != nil {
		return rollbackWindowsCandidate(ctx, backend, journal, err)
	}
	journal.Stage = windowsActivationDraining
	if err = backend.WriteJournal(journal); err != nil {
		return rollbackWindowsCandidate(ctx, backend, journal, err)
	}
	if err = backend.Drain(ctx, journal); err != nil {
		var busy *autoupdate.ActiveTerminalSessionsError
		if errors.As(err, &busy) {
			return compensateWindowsBusy(ctx, backend, journal)
		}
		return rollbackWindowsDrain(ctx, backend, journal, err)
	}
	if err = backend.StopCandidate(ctx, journal); err != nil {
		return rollbackWindowsDrain(ctx, backend, journal, err)
	}
	// Draining is now durable. The old route must be restored if any later
	// service, binary, or health operation fails.
	journal.Stage = windowsActivationSwitching
	if err = backend.WriteJournal(journal); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, err)
	}
	if err = backend.StopServices(ctx, journal.LocalDaemonWasRunning); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, err)
	}
	if err = backend.ActivateBinary(ctx, journal); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, err)
	}
	if err = backend.SetServiceTargets(ctx, journal.NewHostd, journal.NewUpdater, journal.NewSSH); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, err)
	}
	// Every canonical participant must run the new binary before health can
	// verify its version. Rollback restores the recorded prior running state.
	if err = backend.StartServices(ctx, true, true, journal.NewSSH.WasRunning, true); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, err)
	}
	journal.Stage = windowsActivationServicesLive
	if err = backend.WriteJournal(journal); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, err)
	}
	if err = backend.VerifyHealth(ctx, journal); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, err)
	}
	if err = backend.CommitCLI(ctx, journal); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, err)
	}
	journal.Stage, journal.Failure = windowsActivationCommitReady, ""
	if err = backend.WriteJournal(journal); err != nil {
		return journal, err
	}
	return completeWindowsCommit(ctx, backend, journal)
}

// completeWindowsCommit never rolls back a healthy published installation.
// The durable commit-ready stage retains ownership until hostd releases its
// exact admission fence; a failed RPC is retried after helper restart.
func completeWindowsCommit(ctx context.Context, backend windowsActivationBackend, journal windowsActivationJournal) (windowsActivationJournal, error) {
	if journal.Stage == windowsActivationCommitted {
		journal.Stage = windowsActivationCommitReady
		if err := backend.WriteJournal(journal); err != nil {
			return journal, err
		}
	}
	if err := backend.CommitGate(ctx, journal); err != nil {
		return journal, err
	}
	committed := journal
	committed.Stage = windowsActivationCommitted
	if err := backend.WriteJournal(committed); err != nil {
		return journal, err
	}
	return committed, backend.FinalizeServices(ctx, committed)
}

// rollbackWindowsDrain never actuates the canonical process set. Until the
// switching boundary, a missing RPC response may mean a busy live terminal.
func rollbackWindowsDrain(ctx context.Context, backend windowsActivationBackend, journal windowsActivationJournal, cause error) (windowsActivationJournal, error) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), journal.RollbackTimeout)
	defer cancel()
	if err := backend.AuthorizeRecovery(bounded, journal); err != nil {
		return journal, errors.Join(cause, err)
	}
	if err := backend.RollbackDrain(bounded, journal); err != nil {
		return journal, errors.Join(cause, err)
	}
	return rollbackWindowsCandidate(bounded, backend, journal, cause)
}

func rollbackWindowsActivation(ctx context.Context, backend windowsActivationBackend, journal windowsActivationJournal, cause error) (windowsActivationJournal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), journal.RollbackTimeout)
	defer cancel()
	if err := backend.AuthorizeRecovery(ctx, journal); err != nil {
		return journal, errors.Join(cause, err)
	}
	journal.Stage, journal.Failure = windowsActivationRollingBack, boundedWindowsActivationFailure(cause)
	journalErr := backend.WriteJournal(journal)
	stopErr := backend.StopServices(ctx, journal.LocalDaemonWasRunning)
	var targetErr error
	var binaryErr error
	if stopErr == nil {
		binaryErr = backend.RestoreBinary(ctx, journal)
	}
	if stopErr == nil && binaryErr == nil {
		// RestoreBinary moves the previous executable out of the rollback slot
		// and back into the canonical path. If an interrupted transaction had
		// recorded PaperboatUpdated on that rollback slot, restart it from the
		// canonical path now; the slot is intentionally no longer present.
		hostd, updater, ssh, normalizeErr := normalizeWindowsRollbackTargets(journal.OldHostd, journal.OldUpdater, journal.OldSSH)
		if normalizeErr != nil {
			targetErr = normalizeErr
		} else {
			journal.OldUpdater = updater
			targetErr = backend.SetServiceTargets(ctx, hostd, updater, ssh)
		}
	}
	// Restore the durable version and CLI pointer before restarting the old
	// updater. Otherwise the old updater can observe candidate state and race
	// this rollback.
	cliErr := backend.CommitCLI(ctx, windowsActivationJournal{Version: journal.PreviousVersion, PreviousCLIRecord: journal.NewCLIRecord, NewCLIRecord: journal.PreviousCLIRecord})
	quarantineErr := backend.Quarantine(ctx, journal)
	// The binary and SCM target restoration are the safety boundary. Cleanup
	// of the durable CLI record or quarantining the candidate is best effort at
	// this point: a locked candidate (often the activator's own image) must not
	// leave Hostd, SSH, and the updater all stopped. Preserve cleanup errors in
	// the journal/result, but continue to the rollback-ready cut point whenever
	// the machine can safely run the previous binary again.
	criticalErr := errors.Join(journalErr, stopErr, binaryErr, targetErr)
	cleanupErr := errors.Join(cliErr, quarantineErr)
	if criticalErr != nil {
		return journal, errors.Join(cause, criticalErr, cleanupErr)
	}
	// Persist that every mutable pointer is old before starting the old updater.
	// Recovery from this cut point may only finish rollback.
	journal.Stage = windowsActivationRollbackReady
	if cleanupErr != nil {
		journal.Failure = boundedWindowsActivationFailure(cleanupErr)
	}
	if err := backend.WriteJournal(journal); err != nil {
		return journal, errors.Join(cause, cleanupErr, err)
	}
	result, startErr := completeWindowsRollback(ctx, backend, journal, cause)
	return result, errors.Join(startErr, cleanupErr)
}

// compensateWindowsBusy retires only the staged candidate. The host and its
// terminal processes never stopped, so neither binary rollback nor quarantine
// is appropriate. The durable marker makes interrupted cleanup repeatable.
func compensateWindowsBusy(ctx context.Context, backend windowsActivationBackend, journal windowsActivationJournal) (windowsActivationJournal, error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), journal.RollbackTimeout)
	defer cancel()
	journal.Stage, journal.PreDrainRollback = windowsActivationRollingBack, true
	journal.BlockedReason, journal.Failure = autoupdate.BlockedActiveTerminalSessions, ""
	if journal.BlockedRetryAt.IsZero() {
		journal.BlockedRetryAt = time.Now().UTC().Add(autoupdate.DefaultRetryFloor)
	}
	if err := backend.WriteJournal(journal); err != nil {
		return journal, err
	}
	if err := backend.StopCandidate(cleanup, journal); err != nil {
		return journal, err
	}
	if err := backend.AuthorizeRecovery(cleanup, journal); err != nil {
		return journal, err
	}
	journal.Stage = windowsActivationBusyReady
	if err := backend.WriteJournal(journal); err != nil {
		return journal, err
	}
	// Only the updater exited for the activator handoff. Do not touch hostd,
	// managed SSH, or the enrolled user's daemon while its sessions are live.
	if err := backend.StartServices(cleanup, false, journal.OldUpdater.WasRunning, false, false); err != nil {
		return journal, err
	}
	journal.Stage, journal.PreDrainRollback = windowsActivationRolledBack, false
	if err := backend.WriteJournal(journal); err != nil {
		return journal, err
	}
	return journal, &autoupdate.ActiveTerminalSessionsError{RequiredVersion: journal.Version}
}

// rollbackWindowsCandidate aborts a pre-drain candidate without touching the
// currently active services or route. This is intentionally separate from the
// full rollback path: a failed canary must not cause an unnecessary outage or
// turn an undrained route into an uncertain one.
func rollbackWindowsCandidate(ctx context.Context, backend windowsActivationBackend, journal windowsActivationJournal, cause error) (windowsActivationJournal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	journal.Stage = windowsActivationRollingBack
	journal.PreDrainRollback = true
	journal.Failure = boundedWindowsActivationFailure(cause)
	journalErr := backend.WriteJournal(journal)
	stopErr := backend.StopCandidate(ctx, journal)
	quarantineErr := backend.Quarantine(ctx, journal)
	criticalErr := errors.Join(journalErr, stopErr)
	if criticalErr != nil {
		return journal, errors.Join(cause, criticalErr, quarantineErr)
	}
	// The updater that received the update request deliberately hands the
	// transaction to the one-shot activator and exits before this function runs.
	// A pre-drain candidate failure therefore has to restart only the updater
	// before it can publish a terminal rollback. Without this
	// restart, a failed canary leaves PaperboatUpdated stopped even though no
	// binary or service target was changed. Use an independent bounded cleanup
	// context so an activator cancellation cannot strand the old service set.
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), journal.RollbackTimeout)
	defer cancel()
	if err := backend.AuthorizeRecovery(startCtx, journal); err != nil {
		return journal, errors.Join(cause, err, quarantineErr)
	}
	startErr := backend.StartServices(startCtx, false, journal.OldUpdater.WasRunning, false, false)
	cancel()
	if startErr != nil {
		return journal, errors.Join(cause, startErr, quarantineErr)
	}
	journal.Stage = windowsActivationRolledBack
	journal.PreDrainRollback = false
	if err := backend.WriteJournal(journal); err != nil {
		return journal, errors.Join(cause, quarantineErr, err)
	}
	return journal, errors.Join(cause, quarantineErr)
}

func completeWindowsRollback(_ context.Context, backend windowsActivationBackend, journal windowsActivationJournal, cause error) (windowsActivationJournal, error) {
	ctx, cancel := context.WithTimeout(context.Background(), journal.RollbackTimeout)
	defer cancel()
	if journal.Stage != windowsActivationRollbackReady {
		return journal, errors.Join(cause, errInvalidWindowsActivation)
	}
	if err := backend.AuthorizeRecovery(ctx, journal); err != nil {
		return journal, errors.Join(cause, err)
	}
	if err := backend.StartServices(ctx, journal.OldHostd.WasRunning, journal.OldUpdater.WasRunning, journal.OldSSH.WasRunning, journal.LocalDaemonWasRunning); err != nil {
		return journal, errors.Join(cause, err)
	}
	if err := backend.VerifyRollback(ctx, journal); err != nil {
		return journal, errors.Join(cause, err)
	}
	journal.Stage = windowsActivationRolledBack
	if err := backend.WriteJournal(journal); err != nil {
		return journal, errors.Join(cause, err)
	}
	return journal, cause
}

func validWindowsActivationJournal(j windowsActivationJournal) bool {
	if j.ManualMode != "" && j.ManualMode != "update" && j.ManualMode != "maintenance" {
		return false
	}
	if (j.BlockedReason != "" && j.BlockedRetryAt.IsZero()) || (j.BlockedReason == "" && !j.BlockedRetryAt.IsZero()) {
		return false
	}
	if j.Stage == windowsActivationBusyReady && j.BlockedReason != autoupdate.BlockedActiveTerminalSessions {
		return false
	}
	if j.BlockedReason != "" && (j.BlockedReason != autoupdate.BlockedActiveTerminalSessions || j.Failure != "" || !((j.Stage == windowsActivationRollingBack || j.Stage == windowsActivationBusyReady) && j.PreDrainRollback || j.Stage == windowsActivationRolledBack && !j.PreDrainRollback)) {
		return false
	}
	if j.Schema != windowsActivationJournalSchema || len(j.TransactionID) != 32 || !lowerHex(j.TransactionID) || !exactReleasePattern.MatchString(j.Version) || !exactReleasePattern.MatchString(j.PreviousVersion) || !validWindowsActivationStage(j.Stage) || j.Architecture != "amd64" && j.Architecture != "arm64" || len(j.Failure) > 4096 || invalidWindowsAPIRange(j.HostdAPIMin, j.HostdAPIMax) || invalidWindowsAPIRange(j.RuntimeAPIMin, j.RuntimeAPIMax) || workerupdate.ValidateActivationPolicy(windowsCandidateRelease(j)) != nil {
		return false
	}
	for _, component := range []windowsActivationComponent{j.Runtime, j.CLI, j.Hostd, j.Updater, j.PreviousBinary} {
		if component.Path == "" || len(component.SHA256) != 64 || !lowerHex(component.SHA256) || component.Length <= 0 || component.Length > maxWindowsComponentSize {
			return false
		}
	}
	for _, target := range []windowsServiceTarget{j.OldHostd, j.NewHostd} {
		if target.Executable == "" || len(target.Arguments) != 2 || target.Arguments[0] != "daemon" || target.Arguments[1] != "__runtime-hostd" {
			return false
		}
	}
	for _, target := range []windowsServiceTarget{j.OldUpdater, j.NewUpdater} {
		if target.Executable == "" || len(target.Arguments) != 2 || target.Arguments[0] != "daemon" || target.Arguments[1] != "__runtime-updated" {
			return false
		}
	}
	if (j.OldSSH.Executable == "") != (j.NewSSH.Executable == "") || j.OldSSH.Executable != "" && (!validWindowsSSHArguments(j.OldSSH.Arguments) || !validWindowsSSHArguments(j.NewSSH.Arguments)) {
		return false
	}
	// The stable layout.Binary path is the sole CLI/runtime entry point. These
	// fields remain in the journal for decoding old schema-shaped records, but a
	// new transaction must never create or consume a pb.active pointer.
	return j.PreviousCLIRecord == "" && j.NewCLIRecord == ""
}

func invalidWindowsAPIRange(minimum, maximum uint16) bool {
	return minimum == 0 || minimum > maximum || maximum > 1024
}

func validWindowsSSHArguments(arguments []string) bool {
	return len(arguments) == 6 && arguments[0] == "daemon" && arguments[1] == "__windows-sshd-service" && arguments[2] == "--sshd" && strings.EqualFold(arguments[3], `C:\Program Files\OpenSSH\sshd.exe`) && arguments[4] == "--config" && strings.EqualFold(arguments[5], `C:\ProgramData\Paperboat\ssh\sshd_config`)
}

func boundedWindowsActivationFailure(cause error) string {
	if cause == nil {
		return "activation failed"
	}
	const maximum = 2048
	message := strings.Map(func(character rune) rune {
		if character == '\x00' || character == '\r' || character == '\n' {
			return ' '
		}
		return character
	}, cause.Error())
	if len(message) > maximum {
		message = message[:maximum]
	}
	return message
}

func validWindowsActivationStage(stage windowsActivationStage) bool {
	switch stage {
	case windowsActivationCommitReady, windowsActivationBusyReady, windowsActivationStaged, windowsActivationCandidateValidating, windowsActivationCandidateReady, windowsActivationDraining, windowsActivationSwitching, windowsActivationServicesLive, windowsActivationCommitted, windowsActivationRollingBack, windowsActivationRollbackReady, windowsActivationRolledBack:
		return true
	default:
		return false
	}
}

func windowsCandidateRelease(j windowsActivationJournal) workerupdate.Release {
	return workerupdate.Release{Version: j.Version, SHA256: j.Runtime.SHA256, Length: j.Runtime.Length, Platform: "windows", Architecture: j.Architecture, ManifestSHA256: j.ManifestSHA256, CanaryPath: j.CanaryPath, CanaryStatus: j.CanaryStatus, CanarySamples: j.CanarySamples, CanaryTimeout: j.CanaryTimeout, DrainTimeout: j.DrainTimeout, StabilityWindow: j.StabilityWindow, StabilityInterval: j.StabilityInterval, RollbackTimeout: j.RollbackTimeout, HostdAPIMin: j.HostdAPIMin, HostdAPIMax: j.HostdAPIMax, RuntimeAPIMin: j.RuntimeAPIMin, RuntimeAPIMax: j.RuntimeAPIMax}
}

func lowerHex(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func windowsActivationServiceNames(setupMode string) []string {
	if setupMode == "host" {
		return []string{"PaperboatSshd", "PaperboatHostd", "PaperboatUpdated"}
	}
	return []string{"PaperboatHostd", "PaperboatUpdated"}
}

func windowsActivationServiceStartNames(setupMode string, hostd, updater, ssh bool) []string {
	names := make([]string, 0, 3)
	if setupMode == "host" && ssh {
		names = append(names, "PaperboatSshd")
	}
	if hostd {
		names = append(names, "PaperboatHostd")
	}
	if updater {
		names = append(names, "PaperboatUpdated")
	}
	return names
}

func validWindowsSSHRoleTarget(setupMode string, target windowsServiceTarget) bool {
	return setupMode == "host" && target.Executable != "" || setupMode == "client" && target.Executable == ""
}

func windowsActivationBlocksVersion(journal windowsActivationJournal, version string) bool {
	return journal.Stage != windowsActivationCommitted && journal.Stage != windowsActivationRolledBack && (journal.Version == version || journal.PreviousVersion == version)
}

func windowsActivationNeedsControllerRecovery(journal windowsActivationJournal, activeVersion string) bool {
	// Keep the controller's recovery decision aligned with the startup path.
	// Both paths use resumeWindowsActivation, whose predicate covers every
	// nonterminal stage that the activator can safely resume, including an
	// interrupted rolling_back phase. The caller has already established that
	// the journal fences activeVersion; the predicate below preserves the same
	// candidate-version guard used by startup recovery.
	return windowsActivationNeedsResume(journal, activeVersion, false)
}

func windowsActivationNeedsResume(journal windowsActivationJournal, activeVersion string, activatorOwnsTransaction bool) bool {
	if journal.Stage == windowsActivationCommitReady {
		return !activatorOwnsTransaction
	}
	if journal.Stage == windowsActivationCommitted || journal.Stage == windowsActivationRolledBack || activeVersion == journal.Version {
		return false
	}
	return !activatorOwnsTransaction
}
