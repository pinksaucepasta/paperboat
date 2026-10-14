package updated

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
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
	SHA256     string   `json:"sha256,omitempty"`
	Length     int64    `json:"length,omitempty"`
	Arguments  []string `json:"arguments"`
	WasRunning bool     `json:"was_running"`
}

type windowsActivationJournal struct {
	PreviousSessionOwnerRecorded                                  *bool                          `json:"previous_session_owner_recorded,omitempty"`
	Candidate                                                     workerupdate.PreparedCandidate `json:"candidate"`
	ApprovedCandidateID                                           string                         `json:"approved_candidate_id,omitempty"`
	PreviousSource                                                *installsource.Source          `json:",omitempty"`
	Schema, TransactionID, PreviousVersion, Version, Architecture string
	Stage                                                         windowsActivationStage
	Runtime, CLI, Hostd, Updater, PreviousBinary                  windowsActivationComponent
	OldHostd, NewHostd, OldUpdater, NewUpdater, OldSSH, NewSSH    windowsServiceTarget
	PreviousCLIRecord, NewCLIRecord                               string
	LocalDaemonWasRunning                                         bool
	ManualApproval                                                bool `json:"manual_approval,omitempty"` // Records how the candidate was selected.
	Force                                                         bool `json:"force,omitempty"`           // Authorizes interrupting protected workloads.
	Release                                                       workerupdate.Release
	PreviousRuntime                                               windowsActivationComponent
	GateTarget                                                    *hostdproto.UpdateGateTargetBinding
	Failure                                                       string
}

var errInvalidWindowsActivation = errors.New("invalid Windows activation transaction")

func validWindowsActivationJournal(j windowsActivationJournal) bool {
	raw, err := json.Marshal(j.Release)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(raw)
	if j.Candidate.ID != hex.EncodeToString(digest[:]) || j.Candidate.OwnerMaintenance != j.Release.SupervisorMaintenance {
		return false
	}

	if !j.Release.SupervisorMaintenance && (j.PreviousRuntime.Path == "" || j.PreviousRuntime.SHA256 != j.PreviousBinary.SHA256 || j.PreviousRuntime.Length != j.PreviousBinary.Length) {
		return false
	}
	if j.GateTarget != nil && j.GateTarget.Validate() != nil {
		return false
	}
	if j.Release.Version != j.Version || j.Release.Platform != "windows" || j.Release.Architecture != j.Architecture || j.Release.SHA256 != j.Runtime.SHA256 || j.Release.Length != j.Runtime.Length {
		return false
	}
	if j.CLI != j.Runtime || j.Hostd != j.Runtime || j.Updater != j.Runtime {
		return false
	}
	if len(j.Candidate.ID) != 64 || !lowerHex(j.Candidate.ID) || j.Candidate.Version != j.Version || j.Candidate.Platform != "windows" || j.Candidate.Architecture != j.Architecture || j.Candidate.SHA256 != j.Runtime.SHA256 || j.Candidate.Length != j.Runtime.Length || (j.ApprovedCandidateID != "" && j.ApprovedCandidateID != j.Candidate.ID) {
		return false
	}
	if j.Stage != windowsActivationAwaitingApproval && j.ApprovedCandidateID != j.Candidate.ID {
		return false
	}

	if j.PreviousSessionOwnerRecorded != nil && j.PreviousSource == nil {
		return false
	}
	if j.PreviousSource != nil && !validWindowsLocalPrevious(j) {
		return false
	}
	if j.Schema != windowsActivationJournalSchema || len(j.TransactionID) != 32 || !lowerHex(j.TransactionID) || !exactReleasePattern.MatchString(j.Version) || !(exactReleasePattern.MatchString(j.PreviousVersion) || validWindowsLocalPrevious(j)) || !validWindowsActivationStage(j.Stage) || j.Architecture != "amd64" && j.Architecture != "arm64" || len(j.Failure) > 4096 || invalidWindowsAPIRange(j.Release.HostdAPIMin, j.Release.HostdAPIMax) || invalidWindowsAPIRange(j.Release.RuntimeAPIMin, j.Release.RuntimeAPIMax) || workerupdate.ValidateActivationPolicy(windowsCandidateRelease(j)) != nil {
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
	return j.Release
}

func lowerHex(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func windowsActivationServiceNames(instance string) []string {
	return []string{"PaperboatSshd-" + instance, "PaperboatHostd-" + instance, "PaperboatUpdated-" + instance}
}

func windowsActivationServiceStartNames(instance string, hostd, updater, ssh bool) []string {
	names := make([]string, 0, 3)
	if ssh {
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

func validWindowsSSHTarget(target windowsServiceTarget) bool {
	return target.Executable != ""
}

func windowsActivationBlocksVersion(journal windowsActivationJournal, _ string) bool {
	return journal.Stage != windowsActivationCommitted && journal.Stage != windowsActivationRolledBack
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
	if journal.Stage == windowsActivationCommitted || journal.Stage == windowsActivationRolledBack {
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

// nativeWindowsRollbackSource preserves the distinction between explicitly
// installed baselines and signed update bytes when the native installer takes over.
func nativeWindowsRollbackSource(j windowsActivationJournal) (installsource.Source, bool, error) {
	if !nativeWindowsJournalRetirable(j) {
		return installsource.Source{}, false, errInvalidWindowsActivation
	}
	if j.Stage == windowsActivationRolledBack && j.PreviousSource != nil {
		return *j.PreviousSource, j.PreviousSource.Distribution == installsource.Official, nil
	}
	version, component := j.Version, j.Runtime
	if j.Stage == windowsActivationRolledBack {
		version, component = j.PreviousVersion, j.PreviousBinary
	}
	source := installsource.Source{Version: version, Platform: "windows", Architecture: j.Architecture, SHA256: component.SHA256, Length: component.Length, Distribution: installsource.Official, AutomaticUpdates: true}
	if err := source.Validate(); err != nil {
		return installsource.Source{}, false, err
	}
	return source, true, nil
}

func windowsNativeInstallMaySupersede(j windowsActivationJournal) bool {
	return j.Stage == windowsActivationAwaitingApproval && j.ApprovedCandidateID == ""
}

func validateWindowsNativeInstallPreviousSource(source installsource.Source, j windowsActivationJournal) error {
	if source.Version != j.PreviousVersion || source.Platform != "windows" || source.Architecture != j.Architecture || source.SHA256 != j.PreviousBinary.SHA256 || source.Length != j.PreviousBinary.Length || j.PreviousSource != nil && source != *j.PreviousSource {
		return errInvalidWindowsActivation
	}
	return source.Validate()
}
