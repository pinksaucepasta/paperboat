package updated

import (
	"context"
	"encoding/json"
	"errors"

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
	return windowsActivationJournal{Candidate: workerupdate.PreparedCandidate{ID: strings.Repeat("d", 64), Version: "2026.08.23.1", Platform: "windows", Architecture: "amd64", SHA256: c.SHA256, Length: c.Length}, ApprovedCandidateID: strings.Repeat("d", 64), Schema: windowsActivationJournalSchema, TransactionID: strings.Repeat("1", 32), PreviousVersion: "2026.08.22.1", Version: "2026.08.23.1", Architecture: "amd64", Stage: windowsActivationStaged, Runtime: c, CLI: c, Hostd: c, Updater: c, PreviousBinary: previous, OldHostd: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-hostd", "--instance", "u0123456789abcdef01234567"}, WasRunning: true}, NewHostd: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-hostd", "--instance", "u0123456789abcdef01234567"}}, OldUpdater: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-updated", "--instance", "u0123456789abcdef01234567"}, WasRunning: true}, NewUpdater: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-updated", "--instance", "u0123456789abcdef01234567"}}, ManifestSHA256: strings.Repeat("b", 64), CanaryPath: "/_paperboat/update-canary", CanaryStatus: 204, CanarySamples: 3, CanaryTimeout: time.Second, DrainTimeout: time.Second, StabilityWindow: time.Second, StabilityInterval: time.Second, RollbackTimeout: time.Second, HostdAPIMin: 1, HostdAPIMax: 2, RuntimeAPIMin: 1, RuntimeAPIMax: 2}
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

func TestWindowsActivationServiceSetIsRoleScoped(t *testing.T) {
	if got, want := windowsActivationServiceNames("client", "u0123456789abcdef01234567"), []string{"PaperboatHostd-u0123456789abcdef01234567", "PaperboatUpdated-u0123456789abcdef01234567"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("client=%q want=%q", got, want)
	}
	if got, want := windowsActivationServiceNames("host", "u0123456789abcdef01234567"), []string{"PaperboatSshd-u0123456789abcdef01234567", "PaperboatHostd-u0123456789abcdef01234567", "PaperboatUpdated-u0123456789abcdef01234567"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("host=%q want=%q", got, want)
	}
	if got, want := windowsActivationServiceStartNames("host", "u0123456789abcdef01234567", true, true, true), []string{"PaperboatSshd-u0123456789abcdef01234567", "PaperboatHostd-u0123456789abcdef01234567", "PaperboatUpdated-u0123456789abcdef01234567"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("host start order=%q want=%q", got, want)
	}
	if got, want := windowsActivationServiceStartNames("client", "u0123456789abcdef01234567", true, true, true), []string{"PaperboatHostd-u0123456789abcdef01234567", "PaperboatUpdated-u0123456789abcdef01234567"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("client start order=%q want=%q", got, want)
	}
	if got, want := windowsActivationServiceStartNames("host", "u0123456789abcdef01234567", false, true, true), []string{"PaperboatSshd-u0123456789abcdef01234567", "PaperboatUpdated-u0123456789abcdef01234567"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered host start order=%q want=%q", got, want)
	}
	sshArguments := []string{"daemon", "__windows-sshd-service", "--instance", "u0123456789abcdef01234567"}
	if validWindowsSSHArguments(sshArguments[1:]) || validWindowsSSHArguments(append(append([]string(nil), sshArguments...), "extra")) {
		t.Fatal("SSH runtime accepted a missing daemon entry point or extra arguments")
	}
	target := windowsServiceTarget{Executable: "sshd", Arguments: sshArguments}
	if !validWindowsSSHRoleTarget("host", target) || validWindowsSSHRoleTarget("host", windowsServiceTarget{}) || validWindowsSSHRoleTarget("client", target) || !validWindowsSSHRoleTarget("client", windowsServiceTarget{}) {
		t.Fatal("PaperboatSshd role invariant is not exact")
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
