package updated

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

const windowsActivationJournalSchema = "paperboat.windows-activation/v1"
const maxWindowsComponentSize int64 = 256 << 20

type windowsActivationStage string

const (
	windowsActivationAwaitingApproval windowsActivationStage = "awaiting_approval"
	windowsActivationStaged           windowsActivationStage = "staged"
	windowsActivationSwitching        windowsActivationStage = "switching"
	windowsActivationServicesLive     windowsActivationStage = "services_live"
	windowsActivationCommitted        windowsActivationStage = "committed"
	windowsActivationCommitReady      windowsActivationStage = "commit_ready"
	windowsActivationRollingBack      windowsActivationStage = "rolling_back"
	windowsActivationRollbackReady    windowsActivationStage = "rollback_ready"
	windowsActivationRolledBack       windowsActivationStage = "rolled_back"
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
	Candidate                                                     workerupdate.PreparedCandidate `json:"candidate"`
	ApprovedCandidateID                                           string                         `json:"approved_candidate_id,omitempty"`
	PreviousSource                                                *installsource.Source          `json:",omitempty"`
	Schema, TransactionID, PreviousVersion, Version, Architecture string
	Stage                                                         windowsActivationStage
	Runtime, CLI, Hostd, Updater, PreviousBinary                  windowsActivationComponent
	OldHostd, NewHostd, OldUpdater, NewUpdater, OldSSH, NewSSH    windowsServiceTarget
	PreviousCLIRecord, NewCLIRecord                               string
	LocalDaemonWasRunning                                         bool
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
	StopServices(context.Context, bool) error
	ActivateBinary(context.Context, windowsActivationJournal) error
	RestoreBinary(context.Context, windowsActivationJournal) error
	SetServiceTargets(context.Context, windowsServiceTarget, windowsServiceTarget, windowsServiceTarget) error
	StartServices(context.Context, bool, bool, bool, bool) error
	VerifyHealth(context.Context, windowsActivationJournal) error
	VerifyRollback(context.Context, windowsActivationJournal) error
	CommitCLI(context.Context, windowsActivationJournal) error
	VerifyCommitted(context.Context, windowsActivationJournal) error
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
	if journal.Stage == windowsActivationAwaitingApproval {
		return journal, workerupdate.ErrApprovalRequired
	}
	if journal.ApprovedCandidateID == "" || journal.ApprovedCandidateID != journal.Candidate.ID {
		return journal, workerupdate.ErrApprovalRequired
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
	if journal.Stage == windowsActivationRollbackReady {
		return completeWindowsRollback(ctx, backend, journal, errors.New("interrupted rollback recovered"))
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
	journal.Stage = windowsActivationSwitching
	if err = backend.WriteJournal(journal); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, err)
	}
	if err = backend.StopServices(ctx, journal.LocalDaemonWasRunning); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, fmt.Errorf("stop Windows services: %w", err))
	}
	if err = backend.ActivateBinary(ctx, journal); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, fmt.Errorf("activate Windows binary: %w", err))
	}
	if err = backend.SetServiceTargets(ctx, journal.NewHostd, journal.NewUpdater, journal.NewSSH); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, fmt.Errorf("set Windows service targets: %w", err))
	}
	// Every canonical participant must run the new binary before health can
	// verify its version. Rollback restores the recorded prior running state.
	if err = backend.StartServices(ctx, true, true, journal.NewSSH.WasRunning, true); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, fmt.Errorf("start Windows candidate services: %w", err))
	}
	journal.Stage = windowsActivationServicesLive
	if err = backend.WriteJournal(journal); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, err)
	}
	if err = backend.VerifyHealth(ctx, journal); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, fmt.Errorf("verify Windows candidate health: %w", err))
	}
	if err = backend.CommitCLI(ctx, journal); err != nil {
		return rollbackWindowsActivation(ctx, backend, journal, fmt.Errorf("commit Windows installation: %w", err))
	}
	journal.Stage, journal.Failure = windowsActivationCommitReady, ""
	if err = backend.WriteJournal(journal); err != nil {
		return journal, err
	}
	return completeWindowsCommit(ctx, backend, journal)
}

// completeWindowsCommit never rolls back a healthy published installation.
// The durable commit-ready stage retains ownership until installation
// verification finishes; a failed verification is retried after helper restart.
func completeWindowsCommit(ctx context.Context, backend windowsActivationBackend, journal windowsActivationJournal) (windowsActivationJournal, error) {
	if journal.Stage == windowsActivationCommitted {
		journal.Stage = windowsActivationCommitReady
		if err := backend.WriteJournal(journal); err != nil {
			return journal, err
		}
	}
	if err := backend.VerifyCommitted(ctx, journal); err != nil {
		return journal, err
	}
	committed := journal
	committed.Stage = windowsActivationCommitted
	if err := backend.WriteJournal(committed); err != nil {
		return journal, err
	}
	return committed, backend.FinalizeServices(ctx, committed)
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
		// The SCM backend normalizes against the enrolled owner's authoritative
		// layout; the portable transaction does not invent filesystem paths.
		targetErr = backend.SetServiceTargets(ctx, journal.OldHostd, journal.OldUpdater, journal.OldSSH)
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
	if j.CLI != j.Runtime || j.Hostd != j.Runtime || j.Updater != j.Runtime {
		return false
	}
	if len(j.Candidate.ID) != 64 || !lowerHex(j.Candidate.ID) || j.Candidate.Version != j.Version || j.Candidate.Platform != "windows" || j.Candidate.Architecture != j.Architecture || j.Candidate.SHA256 != j.Runtime.SHA256 || j.Candidate.Length != j.Runtime.Length || (j.ApprovedCandidateID != "" && j.ApprovedCandidateID != j.Candidate.ID) {
		return false
	}
	if j.Stage != windowsActivationAwaitingApproval && j.ApprovedCandidateID != j.Candidate.ID {
		return false
	}

	if j.PreviousSource != nil && !validWindowsLocalPrevious(j) {
		return false
	}
	if j.Schema != windowsActivationJournalSchema || len(j.TransactionID) != 32 || !lowerHex(j.TransactionID) || !exactReleasePattern.MatchString(j.Version) || !(exactReleasePattern.MatchString(j.PreviousVersion) || validWindowsLocalPrevious(j)) || !validWindowsActivationStage(j.Stage) || j.Architecture != "amd64" && j.Architecture != "arm64" || len(j.Failure) > 4096 || invalidWindowsAPIRange(j.HostdAPIMin, j.HostdAPIMax) || invalidWindowsAPIRange(j.RuntimeAPIMin, j.RuntimeAPIMax) || workerupdate.ValidateActivationPolicy(windowsCandidateRelease(j)) != nil {
		return false
	}
	for _, component := range []windowsActivationComponent{j.Runtime, j.CLI, j.Hostd, j.Updater, j.PreviousBinary} {
		if component.Path == "" || len(component.SHA256) != 64 || !lowerHex(component.SHA256) || component.Length <= 0 || component.Length > maxWindowsComponentSize {
			return false
		}
	}
	for _, target := range []windowsServiceTarget{j.OldHostd, j.NewHostd} {
		if target.Executable == "" || len(target.Arguments) != 4 || target.Arguments[0] != "daemon" || target.Arguments[1] != "__runtime-hostd" || target.Arguments[2] != "--instance" || len(target.Arguments[3]) != 25 || target.Arguments[3][0] != 'u' || !lowerHex(target.Arguments[3][1:]) {
			return false
		}
	}
	for _, target := range []windowsServiceTarget{j.OldUpdater, j.NewUpdater} {
		if target.Executable == "" || len(target.Arguments) != 4 || target.Arguments[0] != "daemon" || target.Arguments[1] != "__runtime-updated" || target.Arguments[2] != "--instance" || target.Arguments[3] != j.OldHostd.Arguments[3] {
			return false
		}
	}
	if j.NewHostd.Arguments[3] != j.OldHostd.Arguments[3] {
		return false
	}
	if (j.OldSSH.Executable == "") != (j.NewSSH.Executable == "") || j.OldSSH.Executable != "" && (!validWindowsSSHArguments(j.OldSSH.Arguments) || !validWindowsSSHArguments(j.NewSSH.Arguments)) {
		return false
	}
	if j.OldSSH.Executable != "" && (j.OldSSH.Arguments[3] != j.OldHostd.Arguments[3] || j.NewSSH.Arguments[3] != j.OldHostd.Arguments[3]) {
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
	return len(arguments) == 4 && arguments[0] == "daemon" && arguments[1] == "__windows-sshd-service" && arguments[2] == "--instance" && len(arguments[3]) == 25 && arguments[3][0] == 'u' && lowerHex(arguments[3][1:])
}

func isWindowsSSHInstanceService(name string) bool {
	const prefix = "PaperboatSshd-"
	instance, ok := strings.CutPrefix(name, prefix)
	return ok && len(instance) == 25 && instance[0] == 'u' && lowerHex(instance[1:])
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
	case windowsActivationAwaitingApproval, windowsActivationCommitReady, windowsActivationStaged, windowsActivationSwitching, windowsActivationServicesLive, windowsActivationCommitted, windowsActivationRollingBack, windowsActivationRollbackReady, windowsActivationRolledBack:
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

func windowsActivationServiceNames(setupMode, instance string) []string {
	if setupMode == "host" {
		return []string{"PaperboatSshd-" + instance, "PaperboatHostd-" + instance, "PaperboatUpdated-" + instance}
	}
	return []string{"PaperboatHostd-" + instance, "PaperboatUpdated-" + instance}
}

func windowsActivationServiceStartNames(setupMode, instance string, hostd, updater, ssh bool) []string {
	names := make([]string, 0, 3)
	if setupMode == "host" && ssh {
		names = append(names, "PaperboatSshd-"+instance)
	}
	if hostd {
		names = append(names, "PaperboatHostd-"+instance)
	}
	if updater {
		names = append(names, "PaperboatUpdated-"+instance)
	}
	return names
}

func validWindowsSSHRoleTarget(setupMode string, target windowsServiceTarget) bool {
	return setupMode == "host" && target.Executable != "" || setupMode == "client" && target.Executable == ""
}

func windowsActivationBlocksVersion(journal windowsActivationJournal, version string) bool {
	return journal.Stage != windowsActivationAwaitingApproval && journal.Stage != windowsActivationCommitted && journal.Stage != windowsActivationRolledBack && (journal.Version == version || journal.PreviousVersion == version)
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
	if journal.Stage == windowsActivationAwaitingApproval {
		return false
	}
	if journal.Stage == windowsActivationCommitReady {
		return !activatorOwnsTransaction
	}
	if journal.Stage == windowsActivationCommitted || journal.Stage == windowsActivationRolledBack || activeVersion == journal.Version {
		return false
	}
	return !activatorOwnsTransaction
}

func validWindowsLocalPrevious(j windowsActivationJournal) bool {
	s := j.PreviousSource
	return s != nil && s.Validate() == nil && s.Version == j.PreviousVersion && s.Platform == "windows" && s.Architecture == j.Architecture && s.SHA256 == j.PreviousBinary.SHA256 && s.Length == j.PreviousBinary.Length
}

func nativeWindowsJournalRetirable(j windowsActivationJournal) bool {
	return validWindowsActivationJournal(j) && (j.Stage == windowsActivationCommitted || j.Stage == windowsActivationRolledBack)
}
