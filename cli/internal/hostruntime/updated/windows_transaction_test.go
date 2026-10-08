package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type recordingWindowsActivationBackend struct {
	events              []string
	fail                string
	failStartOnRollback bool
	stoppedLocalDaemon  bool
	startedLocalDaemon  bool
}

func (b *recordingWindowsActivationBackend) event(name string) error {
	b.events = append(b.events, name)
	if b.fail == name {
		return errors.New("injected " + name)
	}
	return nil
}
func (b *recordingWindowsActivationBackend) AuthorizeRecovery(context.Context, windowsActivationJournal) error {
	return nil
}
func (b *recordingWindowsActivationBackend) WriteJournal(j windowsActivationJournal) error {
	return b.event("journal:" + string(j.Stage))
}
func (b *recordingWindowsActivationBackend) ProbeCandidate(_ context.Context, _ windowsActivationJournal) error {
	return b.event("candidate")
}
func (b *recordingWindowsActivationBackend) StopCandidate(_ context.Context, _ windowsActivationJournal) error {
	return b.event("candidate_stop")
}
func (b *recordingWindowsActivationBackend) StopServices(_ context.Context, localDaemon bool) error {
	b.stoppedLocalDaemon = b.stoppedLocalDaemon || localDaemon
	return b.event("stop")
}
func (b *recordingWindowsActivationBackend) ActivateBinary(_ context.Context, _ windowsActivationJournal) error {
	return b.event("activate")
}
func (b *recordingWindowsActivationBackend) RestoreBinary(_ context.Context, _ windowsActivationJournal) error {
	return b.event("restore")
}
func (b *recordingWindowsActivationBackend) SetServiceTargets(_ context.Context, h, _, _ windowsServiceTarget) error {
	return b.event("targets:" + h.Executable)
}
func (b *recordingWindowsActivationBackend) StartServices(_ context.Context, h, u, ssh, localDaemon bool) error {
	b.startedLocalDaemon = b.startedLocalDaemon || localDaemon
	if b.failStartOnRollback && slices.Contains(b.events, "journal:rolling_back") {
		b.events = append(b.events, "start")
		return errors.New("injected start")
	}
	return b.event("start")
}
func (b *recordingWindowsActivationBackend) VerifyHealth(context.Context, windowsActivationJournal) error {
	if !b.startedLocalDaemon {
		return errors.New("local daemon not running before health")
	}
	return b.event("health")
}
func (b *recordingWindowsActivationBackend) Drain(context.Context, windowsActivationJournal) error {
	return b.event("drain")
}
func (b *recordingWindowsActivationBackend) VerifyRollback(context.Context, windowsActivationJournal) error {
	return b.event("verifyRollback")
}
func (b *recordingWindowsActivationBackend) CommitCLI(_ context.Context, j windowsActivationJournal) error {
	return b.event("cli:" + j.NewCLIRecord)
}
func (b *recordingWindowsActivationBackend) Quarantine(context.Context, windowsActivationJournal) error {
	return b.event("quarantine")
}
func (b *recordingWindowsActivationBackend) FinalizeServices(_ context.Context, _ windowsActivationJournal) error {
	b.startedLocalDaemon = true
	return b.event("finalize")
}

func testWindowsActivationJournal() windowsActivationJournal {
	c := windowsActivationComponent{Path: `C:\Paperboat\candidate.exe`, SHA256: strings.Repeat("a", 64), Length: 1}
	previous := c
	previous.Path = `C:\Program Files\Paperboat\bin\pb.exe`
	j := windowsActivationJournal{Candidate: workerupdate.PreparedCandidate{ID: strings.Repeat("d", 64), Version: "2026.08.23.1", Platform: "windows", Architecture: "amd64", SHA256: c.SHA256, Length: c.Length}, ApprovedCandidateID: strings.Repeat("d", 64), Schema: windowsActivationJournalSchema, TransactionID: strings.Repeat("1", 32), PreviousVersion: "2026.08.22.1", Version: "2026.08.23.1", Architecture: "amd64", Stage: windowsActivationStaged, Runtime: c, CLI: c, Hostd: c, Updater: c, PreviousBinary: previous, OldHostd: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-hostd", "--instance", "u0123456789abcdef01234567"}, WasRunning: true}, NewHostd: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-hostd", "--instance", "u0123456789abcdef01234567"}}, OldUpdater: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-updated", "--instance", "u0123456789abcdef01234567"}, WasRunning: true}, NewUpdater: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-updated", "--instance", "u0123456789abcdef01234567"}}, Release: workerupdate.Release{SupervisorMaintenance: true, Version: "2026.08.23.1", Platform: "windows", Architecture: "amd64", SHA256: c.SHA256, Length: c.Length, ManifestSHA256: strings.Repeat("b", 64), CanaryPath: "/_paperboat/update-canary", CanaryStatus: 204, CanarySamples: 3, CanaryTimeout: time.Second, DrainTimeout: time.Second, StabilityWindow: time.Second, StabilityInterval: time.Second, RollbackTimeout: time.Second, HostdAPIMin: 1, HostdAPIMax: 2, RuntimeAPIMin: 1, RuntimeAPIMax: 2}}
	bindWindowsTestCandidate(&j)
	return j
}

func TestWindowsActivationCommitsCLIOnlyAfterHealth(t *testing.T) {
	b := &recordingWindowsActivationBackend{}
	result, err := executeWindowsActivation(context.Background(), b, testWindowsActivationJournal())
	if err != nil || result.Stage != windowsActivationCommitted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := []string{"journal:switching", "stop", "activate", "targets:C:\\Program Files\\Paperboat\\bin\\pb.exe", "start", "journal:services_live", "health", "cli:", "journal:commit_ready", "verifyCommitted", "journal:committed", "finalize"}
	if !reflect.DeepEqual(b.events, want) {
		t.Fatalf("events=%q want=%q", b.events, want)
	}
}

func TestWindowsActivationSuspendsAndRestartsRecordedLocalDaemon(t *testing.T) {
	b := &recordingWindowsActivationBackend{}
	journal := testWindowsActivationJournal()
	journal.LocalDaemonWasRunning = true
	result, err := executeWindowsActivation(context.Background(), b, journal)
	if err != nil || result.Stage != windowsActivationCommitted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !b.stoppedLocalDaemon || !b.startedLocalDaemon {
		t.Fatalf("local daemon lifecycle: stopped=%t started=%t", b.stoppedLocalDaemon, b.startedLocalDaemon)
	}
}

func TestWindowsActivationHealthFailureRestoresExactOldTargetsAndCLI(t *testing.T) {
	b := &recordingWindowsActivationBackend{fail: "health"}
	result, err := executeWindowsActivation(context.Background(), b, testWindowsActivationJournal())
	if err == nil || result.Stage != windowsActivationRolledBack {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	wantTail := []string{"journal:rolling_back", "stop", "restore", "targets:C:\\Program Files\\Paperboat\\bin\\pb.exe", "cli:", "quarantine", "journal:rollback_ready", "start", "verifyRollback", "journal:rolled_back"}
	if !reflect.DeepEqual(b.events[len(b.events)-len(wantTail):], wantTail) {
		t.Fatalf("events=%q", b.events)
	}
}

func TestWindowsActivationRollbackReadyResumeOnlyStartsOldServices(t *testing.T) {
	journal := testWindowsActivationJournal()
	journal.Stage = windowsActivationRollbackReady
	b := &recordingWindowsActivationBackend{}
	result, err := executeWindowsActivation(context.Background(), b, journal)
	if err == nil || result.Stage != windowsActivationRolledBack {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := []string{"start", "verifyRollback", "journal:rolled_back"}
	if !reflect.DeepEqual(b.events, want) {
		t.Fatalf("events=%q want=%q", b.events, want)
	}
}

func TestWindowsActivationAmbiguousRollbackReadyRetainsStrictRollbackGate(t *testing.T) {
	journal := testWindowsActivationJournal()
	journal.Stage = windowsActivationRollbackReady
	b := &recordingWindowsActivationBackend{}
	if _, err := executeWindowsActivation(context.Background(), b, journal); err == nil {
		t.Fatal("expected recovery cause")
	}
	if !slices.Contains(b.events, "verifyRollback") || slices.Contains(b.events, "candidate_stop") {
		t.Fatalf("events=%q", b.events)
	}
}

func TestWindowsActivationDoesNotStartOldUpdaterBeforeRollbackReadyIsDurable(t *testing.T) {
	b := &recordingWindowsActivationBackend{fail: "journal:rollback_ready"}
	result, err := rollbackWindowsActivation(context.Background(), b, testWindowsActivationJournal(), errors.New("candidate failed"))
	if err == nil || result.Stage != windowsActivationRollbackReady || slices.Contains(b.events, "start") {
		t.Fatalf("result=%+v events=%q err=%v", result, b.events, err)
	}
}

func TestWindowsActivationRollbackStartsServicesWhenCleanupFails(t *testing.T) {
	b := &recordingWindowsActivationBackend{fail: "quarantine"}
	result, err := rollbackWindowsActivation(context.Background(), b, testWindowsActivationJournal(), errors.New("candidate failed"))
	if err == nil || result.Stage != windowsActivationRolledBack {
		t.Fatalf("result=%+v events=%q err=%v", result, b.events, err)
	}
	if !slices.Contains(b.events, "start") {
		t.Fatalf("services were not restarted after non-critical cleanup failure: %q", b.events)
	}
}

func TestWindowsActivationFailureIsBoundedAndSingleLine(t *testing.T) {
	message := boundedWindowsActivationFailure(errors.New(strings.Repeat("x", 3000) + "\r\nsecret"))
	if len(message) != 2048 || strings.ContainsAny(message, "\r\n\x00") {
		t.Fatalf("failure length=%d value=%q", len(message), message)
	}
}

func TestWindowsActivationRecoveryNeverContinuesAmbiguousCutover(t *testing.T) {
	j := testWindowsActivationJournal()
	j.Stage = windowsActivationServicesLive
	b := &recordingWindowsActivationBackend{}
	result, err := executeWindowsActivation(context.Background(), b, j)
	if err == nil || result.Stage != windowsActivationRolledBack || b.events[0] != "journal:rolling_back" {
		t.Fatalf("result=%+v events=%q err=%v", result, b.events, err)
	}
}

func TestWindowsActivationRollbackNeverRestartsAfterTargetFailure(t *testing.T) {
	b := &recordingWindowsActivationBackend{fail: `targets:C:\Program Files\Paperboat\bin\pb.exe`}
	result, err := rollbackWindowsActivation(context.Background(), b, testWindowsActivationJournal(), errors.New("candidate failed"))
	if err == nil || result.Stage != windowsActivationRollingBack {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if slices.Contains(b.events, "start") {
		t.Fatalf("unsafe restart after target failure: %q", b.events)
	}
}

func TestWindowsActivationServiceSetAndSSHPrerequisite(t *testing.T) {

	if got, want := windowsActivationServiceNames("u0123456789abcdef01234567"), []string{"PaperboatSshd-u0123456789abcdef01234567", "PaperboatHostd-u0123456789abcdef01234567", "PaperboatUpdated-u0123456789abcdef01234567"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("host=%q want=%q", got, want)
	}
	if got, want := windowsActivationServiceStartNames("u0123456789abcdef01234567", true, true, true), []string{"PaperboatSshd-u0123456789abcdef01234567", "PaperboatHostd-u0123456789abcdef01234567", "PaperboatUpdated-u0123456789abcdef01234567"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("host start order=%q want=%q", got, want)
	}

	if got, want := windowsActivationServiceStartNames("u0123456789abcdef01234567", false, true, true), []string{"PaperboatSshd-u0123456789abcdef01234567", "PaperboatUpdated-u0123456789abcdef01234567"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered host start order=%q want=%q", got, want)
	}
	sshArguments := []string{"daemon", "__windows-sshd-service", "--instance", "u0123456789abcdef01234567"}
	if validWindowsSSHArguments(sshArguments[1:]) || validWindowsSSHArguments(append(append([]string(nil), sshArguments...), "extra")) {
		t.Fatal("SSH runtime accepted a missing daemon entry point or extra arguments")
	}
	target := windowsServiceTarget{Executable: "sshd", Arguments: sshArguments}
	if !validWindowsSSHTarget(target) || validWindowsSSHTarget(windowsServiceTarget{}) {
		t.Fatal("PaperboatSshd target invariant is not exact")
	}
	journal := testWindowsActivationJournal()
	journal.OldSSH, journal.NewSSH = target, target
	if !validWindowsActivationJournal(journal) {
		t.Fatal("exact PaperboatSshd journal rejected")
	}
	journal.NewSSH.Arguments = append([]string(nil), sshArguments...)
	journal.NewSSH.Arguments[3] = "u1"
	if validWindowsActivationJournal(journal) {
		t.Fatal("malformed PaperboatSshd journal accepted")
	}
}

func TestWindowsActivationBlocksBothSidesUntilTerminalJournal(t *testing.T) {
	journal := testWindowsActivationJournal()
	for _, stage := range []windowsActivationStage{windowsActivationStaged, windowsActivationSwitching, windowsActivationServicesLive, windowsActivationRollingBack} {
		journal.Stage = stage
		if !windowsActivationBlocksVersion(journal, journal.Version) || !windowsActivationBlocksVersion(journal, journal.PreviousVersion) {
			t.Fatalf("stage %q did not fence both updater versions", stage)
		}
	}
	journal.Stage = windowsActivationCommitted
	if windowsActivationBlocksVersion(journal, journal.Version) {
		t.Fatal("committed activation remains fenced")
	}
}

func TestWindowsUpdaterDoesNotResumeTransactionOwnedByRunningActivator(t *testing.T) {
	journal := testWindowsActivationJournal()
	for _, stage := range []windowsActivationStage{windowsActivationSwitching, windowsActivationServicesLive, windowsActivationRollingBack, windowsActivationRollbackReady} {
		journal.Stage = stage
		if windowsActivationNeedsResume(journal, journal.PreviousVersion, true) {
			t.Fatalf("stage %q resumed while activator owned transaction", stage)
		}
		if !windowsActivationNeedsResume(journal, journal.PreviousVersion, false) {
			t.Fatalf("stage %q did not resume after activator stopped", stage)
		}
	}
	journal.Stage = windowsActivationCommitted
	if windowsActivationNeedsResume(journal, journal.PreviousVersion, false) {
		t.Fatal("committed transaction resumed")
	}
	journal.Stage = windowsActivationStaged
	if windowsActivationNeedsResume(journal, journal.Version, false) {
		t.Fatal("active candidate version resumed its own transaction")
	}
}

func (b *recordingWindowsActivationBackend) RollbackDrain(context.Context, windowsActivationJournal) error {
	return b.event("rollbackDrain")
}

func TestWindowsJournalOmitsAbsentAdmissionFields(t *testing.T) {
	j := testWindowsActivationJournal()
	j.Stage = windowsActivationRolledBack
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "BlockedReason") || strings.Contains(string(raw), "BlockedRetryAt") {
		t.Fatalf("absent admission fields emitted: %s", raw)
	}
	b := &recordingWindowsActivationBackend{}
	got, err := executeWindowsActivation(context.Background(), b, j)
	if err != nil || !reflect.DeepEqual(got, j) || !reflect.DeepEqual(b.events, []string{"journal:rolled_back"}) {
		t.Fatalf("terminal normalization changed state: %v %v", b.events, err)
	}
}

func TestWindowsSuccessiveUpdatesReleaseAdmission(t *testing.T) {
	b := &recordingWindowsActivationBackend{}
	j := testWindowsActivationJournal()
	for _, id := range []string{strings.Repeat("1", 32), strings.Repeat("2", 32)} {
		j.TransactionID = id
		result, err := executeWindowsActivation(context.Background(), b, j)
		if err != nil || result.Stage != windowsActivationCommitted {
			t.Fatalf("stage=%s err=%v events=%v", result.Stage, err, b.events)
		}
	}
}

func (b *recordingWindowsActivationBackend) VerifyCommitted(context.Context, windowsActivationJournal) error {
	return b.event("verifyCommitted")
}

func TestWindowsCommitFailureResumesWithoutRollback(t *testing.T) {
	for _, fail := range []string{"verifyCommitted", "journal:committed"} {
		t.Run(fail, func(t *testing.T) {
			b := &recordingWindowsActivationBackend{}
			b.fail = fail
			j := testWindowsActivationJournal()
			result, err := executeWindowsActivation(context.Background(), b, j)
			if err == nil || result.Stage != windowsActivationCommitReady || slices.Contains(b.events, "restore") || slices.Contains(b.events, "quarantine") {
				t.Fatalf("stage=%s err=%v events=%v", result.Stage, err, b.events)
			}
			if !windowsActivationNeedsResume(result, j.Version, false) {
				t.Fatal("candidate updater cannot resume commit")
			}
			b.fail = ""
			b.events = nil
			result, err = executeWindowsActivation(context.Background(), b, result)
			if err != nil || result.Stage != windowsActivationCommitted || slices.Contains(b.events, "stop") {
				t.Fatalf("resume=%s err=%v events=%v", result.Stage, err, b.events)
			}
		})
	}
}

func TestWindowsActivationJournalRejectsCrossInstanceTargets(t *testing.T) {
	for _, target := range []string{"hostd", "old SSH", "new SSH"} {
		t.Run(target, func(t *testing.T) {
			journal := testWindowsActivationJournal()
			journal.OldSSH = windowsServiceTarget{Executable: journal.OldHostd.Executable, Arguments: []string{"daemon", "__windows-sshd-service", "--instance", journal.OldHostd.Arguments[3]}}
			journal.NewSSH = windowsServiceTarget{Executable: journal.NewHostd.Executable, Arguments: append([]string(nil), journal.OldSSH.Arguments...)}
			if !validWindowsActivationJournal(journal) {
				t.Fatal("invalid test fixture")
			}
			const foreign = "u1123456789abcdef01234567"
			switch target {
			case "hostd":
				journal.NewHostd.Arguments = append([]string(nil), journal.NewHostd.Arguments...)
				journal.NewHostd.Arguments[3] = foreign
			case "old SSH":
				journal.OldSSH.Arguments = append([]string(nil), journal.OldSSH.Arguments...)
				journal.OldSSH.Arguments[3] = foreign
			case "new SSH":
				journal.NewSSH.Arguments = append([]string(nil), journal.NewSSH.Arguments...)
				journal.NewSSH.Arguments[3] = foreign
			}
			if validWindowsActivationJournal(journal) {
				t.Fatal("accepted cross-user service target")
			}
		})
	}
}

func TestWindowsSSHInstanceServiceRecoverySelection(t *testing.T) {
	for _, test := range []struct {
		name string
		want bool
	}{
		{"PaperboatSshd-u4c8e2991570c314b650297e5", true},
		{"PaperboatSshd-u000000000000000000000000", true},
		{"PaperboatSshd", false},
		{"PaperboatUpdated-u4c8e2991570c314b650297e5", false},
		{"PaperboatSshd-u4c8e2991570c314b650297e5-extra", false},
		{"PaperboatSshd-u4C8e2991570c314b650297e5", false},
		{"PaperboatSshd-u4c8e2991570c314b650297e", false},
		{"PaperboatSshd-", false},
	} {
		if got := isWindowsSSHInstanceService(test.name); got != test.want {
			t.Errorf("service %q: got %v, want %v", test.name, got, test.want)
		}
	}
}

func TestWindowsPreparedCandidateCannotExecuteOrResume(t *testing.T) {
	for _, approved := range []bool{false, true} {
		j := testWindowsActivationJournal()
		j.Stage = windowsActivationAwaitingApproval
		if !approved {
			j.ApprovedCandidateID = ""
		}
		if !validWindowsActivationJournal(j) {
			t.Fatal("prepared candidate rejected")
		}
		if windowsActivationNeedsResume(j, j.PreviousVersion, false) || windowsActivationBlocksVersion(j, j.PreviousVersion) {
			t.Fatal("prepared candidate requests activation")
		}
		backend := &recordingWindowsActivationBackend{}
		result, err := executeWindowsActivation(context.Background(), backend, j)
		if !errors.Is(err, workerupdate.ErrApprovalRequired) || result.Stage != windowsActivationAwaitingApproval || len(backend.events) != 0 {
			t.Fatalf("result=%+v err=%v events=%v", result, err, backend.events)
		}
	}
}

func TestWindowsActivationRejectsApprovalAndArtifactIdentityMismatch(t *testing.T) {
	for _, mutate := range []func(*windowsActivationJournal){
		func(j *windowsActivationJournal) { j.ApprovedCandidateID = "" },
		func(j *windowsActivationJournal) { j.ApprovedCandidateID = strings.Repeat("e", 64) },
		func(j *windowsActivationJournal) { j.Candidate.SHA256 = strings.Repeat("e", 64) },
		func(j *windowsActivationJournal) { j.Candidate.Version = "2026.08.24.1" },
	} {
		j := testWindowsActivationJournal()
		mutate(&j)
		backend := &recordingWindowsActivationBackend{}
		if _, err := executeWindowsActivation(context.Background(), backend, j); err == nil || len(backend.events) != 0 {
			t.Fatalf("invalid candidate executed: %v %v", err, backend.events)
		}
	}
}

func TestWindowsActivationPersistsFailedPhase(t *testing.T) {
	b := &recordingWindowsActivationBackend{fail: "activate"}
	result, err := executeWindowsActivation(context.Background(), b, testWindowsActivationJournal())
	if err == nil || !strings.Contains(err.Error(), "activate Windows binary:") || !strings.Contains(result.Failure, "activate Windows binary:") || result.Stage != windowsActivationRolledBack {
		t.Fatalf("stage=%s failure=%s err=%v", result.Stage, result.Failure, err)
	}
}

func (b *recordingWindowsActivationBackend) AuthorizeOwnerMaintenance(context.Context, workerupdate.Release, bool) error {
	return nil
}

type featureWindowsBackend struct {
	recordingWindowsActivationBackend
}

func (b *featureWindowsBackend) PrepareFeature(context.Context, windowsActivationJournal) (hostdproto.UpdateGateTargetBinding, error) {
	return hostdproto.UpdateGateTargetBinding{Scope: hostdproto.UpdateGateScopeStandalone, MachineID: "machine-one", FailureDomain: "standalone"}, b.event("feature:prepare")
}
func (b *featureWindowsBackend) ActivateFeature(context.Context, windowsActivationJournal) error {
	return b.event("feature:activate")
}
func (b *featureWindowsBackend) RestoreFeature(context.Context, windowsActivationJournal) error {
	return b.event("feature:restore")
}
func (b *featureWindowsBackend) VerifyFeature(context.Context, windowsActivationJournal) error {
	return b.event("feature:health")
}
func (b *featureWindowsBackend) CompleteFeature(context.Context, windowsActivationJournal) error {
	return b.event("feature:commit")
}
func testWindowsFeatureJournal() windowsActivationJournal {
	j := testWindowsActivationJournal()
	j.Release.SupervisorMaintenance = false
	bindWindowsTestCandidate(&j)
	j.PreviousRuntime = j.PreviousBinary
	j.PreviousRuntime.Path = `C:\Paperboat\versions\2026.08.22.1\pb.exe`
	return j
}
func TestWindowsFeatureActivationPreservesNativeOwnersOnCommitAndHealthRollback(t *testing.T) {
	for _, failure := range []string{"", "feature:health"} {
		t.Run(failure, func(t *testing.T) {
			b := &featureWindowsBackend{}
			b.fail = failure
			j, err := executeWindowsActivation(context.Background(), b, testWindowsFeatureJournal())
			if failure == "" {
				if err != nil || j.Stage != windowsActivationCommitted {
					t.Fatalf("commit=%s err=%v", j.Stage, err)
				}
			} else if err == nil || j.Stage != windowsActivationRolledBack {
				t.Fatalf("rollback=%s err=%v", j.Stage, err)
			}
			for _, event := range b.events {
				if event == "stop" || event == "start" || strings.HasPrefix(event, "targets:") {
					t.Fatalf("feature update touched native owner: %q", b.events)
				}
			}
			if failure != "" && !slices.Contains(b.events, "feature:restore") {
				t.Fatalf("trusted feature not restored: %q", b.events)
			}
		})
	}
}
func TestWindowsFeatureInterruptedSwitchRestoresFeatureWithoutOwnerStop(t *testing.T) {
	b := &featureWindowsBackend{}
	j := testWindowsFeatureJournal()
	j.Stage = windowsActivationSwitching
	result, err := executeWindowsActivation(context.Background(), b, j)
	if err == nil || result.Stage != windowsActivationRolledBack || !slices.Contains(b.events, "feature:restore") || slices.Contains(b.events, "stop") {
		t.Fatalf("stage=%s events=%q err=%v", result.Stage, b.events, err)
	}
}

func bindWindowsTestCandidate(j *windowsActivationJournal) {
	raw, _ := json.Marshal(j.Release)
	digest := sha256.Sum256(raw)
	j.Candidate.ID = hex.EncodeToString(digest[:])
	j.Candidate.OwnerMaintenance = j.Release.SupervisorMaintenance
	j.ApprovedCandidateID = j.Candidate.ID
}
func TestWindowsActivationRejectsPolicyMutationAfterApproval(t *testing.T) {
	j := testWindowsActivationJournal()
	j.Release.OwnerMaintenanceGraceSeconds++
	if validWindowsActivationJournal(j) {
		t.Fatal("changed signed policy retained old approval")
	}
}

func (b *recordingWindowsActivationBackend) AbortOwnerMaintenance(context.Context) error {
	return b.event("maintenance:abort")
}
func (b *featureWindowsBackend) AbortFeature(context.Context, windowsActivationJournal) error {
	return b.event("feature:abort")
}
func TestWindowsActivationStorageFailureBeforeOwnerStopAbortsFence(t *testing.T) {
	b := &recordingWindowsActivationBackend{fail: "journal:switching"}
	_, err := executeWindowsActivation(context.Background(), b, testWindowsActivationJournal())
	if err == nil || !slices.Contains(b.events, "maintenance:abort") || slices.Contains(b.events, "stop") {
		t.Fatalf("events=%v err=%v", b.events, err)
	}
}
func TestWindowsFeaturePreparationFailureReleasesGateWithoutReplacement(t *testing.T) {
	b := &featureWindowsBackend{recordingWindowsActivationBackend: recordingWindowsActivationBackend{fail: "feature:prepare"}}
	j := testWindowsActivationJournal()
	j.Release.SupervisorMaintenance = false
	j.PreviousRuntime = j.PreviousBinary
	bindWindowsTestCandidate(&j)
	_, err := executeWindowsActivation(context.Background(), b, j)
	if err == nil || !slices.Contains(b.events, "feature:abort") || slices.Contains(b.events, "feature:activate") {
		t.Fatalf("events=%v err=%v", b.events, err)
	}
}
