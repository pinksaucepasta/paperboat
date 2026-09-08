package updated

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
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
	return windowsActivationJournal{Schema: windowsActivationJournalSchema, TransactionID: strings.Repeat("1", 32), PreviousVersion: "2026.08.22.1", Version: "2026.08.23.1", Architecture: "amd64", Stage: windowsActivationStaged, Runtime: c, CLI: c, Hostd: c, Updater: c, PreviousBinary: previous, OldHostd: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-hostd"}, WasRunning: true}, NewHostd: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-hostd"}}, OldUpdater: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-updated"}, WasRunning: true}, NewUpdater: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-updated"}}, ManifestSHA256: strings.Repeat("b", 64), CanaryPath: "/_paperboat/update-canary", CanaryStatus: 204, CanarySamples: 3, CanaryTimeout: time.Second, DrainTimeout: time.Second, StabilityWindow: time.Second, StabilityInterval: time.Second, RollbackTimeout: time.Second, HostdAPIMin: 1, HostdAPIMax: 2, RuntimeAPIMin: 1, RuntimeAPIMax: 2}
}

func TestWindowsActivationCommitsCLIOnlyAfterHealth(t *testing.T) {
	b := &recordingWindowsActivationBackend{}
	result, err := executeWindowsActivation(context.Background(), b, testWindowsActivationJournal())
	if err != nil || result.Stage != windowsActivationCommitted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := []string{"journal:candidate_validating", "candidate", "journal:candidate_ready", "journal:draining", "drain", "candidate_stop", "journal:switching", "stop", "activate", "targets:C:\\Program Files\\Paperboat\\bin\\pb.exe", "start", "journal:services_live", "health", "cli:", "journal:commit_ready", "gate_commit", "journal:committed", "finalize"}
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

func TestWindowsActivationResumesInterruptedPreDrainRollbackWithoutRollbackGate(t *testing.T) {
	for _, stage := range []windowsActivationStage{windowsActivationRollingBack, windowsActivationRollbackReady} {
		t.Run(string(stage), func(t *testing.T) {
			journal := testWindowsActivationJournal()
			journal.Stage = stage
			journal.PreDrainRollback = true
			b := &recordingWindowsActivationBackend{}
			result, err := executeWindowsActivation(context.Background(), b, journal)
			if err == nil || result.Stage != windowsActivationRolledBack || result.PreDrainRollback {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if slices.Contains(b.events, "verifyRollback") || !slices.Contains(b.events, "candidate_stop") || !slices.Contains(b.events, "start") {
				t.Fatalf("events=%q", b.events)
			}
		})
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

func TestWindowsActivationCandidateFailureLeavesOldRouteUndrained(t *testing.T) {
	b := &recordingWindowsActivationBackend{fail: "candidate"}
	result, err := executeWindowsActivation(context.Background(), b, testWindowsActivationJournal())
	if err == nil || result.Stage != windowsActivationRolledBack {
		t.Fatalf("result=%+v events=%q err=%v", result, b.events, err)
	}
	if slices.Contains(b.events, "drain") || slices.Contains(b.events, "stop") || slices.Contains(b.events, "restore") {
		t.Fatalf("candidate failure touched old route/services: %q", b.events)
	}
	wantTail := []string{"journal:candidate_validating", "candidate", "journal:rolling_back", "candidate_stop", "quarantine", "start", "journal:rolled_back"}
	if len(b.events) < len(wantTail) || !reflect.DeepEqual(b.events[len(b.events)-len(wantTail):], wantTail) {
		t.Fatalf("events=%q want tail=%q", b.events, wantTail)
	}
}

func TestWindowsActivationCandidateRollbackRestartsOnlyUpdater(t *testing.T) {
	b := &recordingWindowsActivationBackend{fail: "candidate"}
	journal := testWindowsActivationJournal()
	journal.LocalDaemonWasRunning = true
	result, err := executeWindowsActivation(context.Background(), b, journal)
	if err == nil || result.Stage != windowsActivationRolledBack {
		t.Fatalf("result=%+v events=%q err=%v", result, b.events, err)
	}
	start := slices.Index(b.events, "start")
	terminal := slices.Index(b.events, "journal:rolled_back")
	if start < 0 || terminal < 0 || start >= terminal || b.startedLocalDaemon {
		t.Fatalf("previous services were not restored before terminal rollback: %q", b.events)
	}
}

func TestWindowsActivationCandidateRollbackRemainsRecoverableIfRestartFails(t *testing.T) {
	b := &recordingWindowsActivationBackend{fail: "candidate", failStartOnRollback: true}
	result, err := executeWindowsActivation(context.Background(), b, testWindowsActivationJournal())
	if err == nil || result.Stage != windowsActivationRollingBack {
		t.Fatalf("result=%+v events=%q err=%v", result, b.events, err)
	}
	if slices.Contains(b.events, "journal:rolled_back") {
		t.Fatalf("published terminal rollback after previous services failed to restart: %q", b.events)
	}
}

func TestWindowsActivationCandidateStagesRecoverWithoutDrainingOldRoute(t *testing.T) {
	for _, stage := range []windowsActivationStage{windowsActivationCandidateValidating, windowsActivationCandidateReady} {
		b := &recordingWindowsActivationBackend{}
		journal := testWindowsActivationJournal()
		journal.Stage = stage
		result, err := executeWindowsActivation(context.Background(), b, journal)
		if err == nil || result.Stage != windowsActivationRolledBack {
			t.Fatalf("stage=%q result=%+v events=%q err=%v", stage, result, b.events, err)
		}
		if slices.Contains(b.events, "drain") || slices.Contains(b.events, "stop") || slices.Contains(b.events, "restore") {
			t.Fatalf("stage=%q touched old route/services: %q", stage, b.events)
		}
	}
}

func TestWindowsActivationJournalOrdersDrainBeforeSwitch(t *testing.T) {
	b := &recordingWindowsActivationBackend{}
	_, err := executeWindowsActivation(context.Background(), b, testWindowsActivationJournal())
	if err != nil {
		t.Fatal(err)
	}
	drainIndex, switchIndex := slices.Index(b.events, "journal:draining"), slices.Index(b.events, "journal:switching")
	if drainIndex < 0 || switchIndex < 0 || drainIndex >= switchIndex {
		t.Fatalf("events=%q: drain must be durable before switching", b.events)
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
	if got, want := windowsActivationServiceNames("client"), []string{"PaperboatHostd", "PaperboatUpdated"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("client=%q want=%q", got, want)
	}
	if got, want := windowsActivationServiceNames("host"), []string{"PaperboatSshd", "PaperboatHostd", "PaperboatUpdated"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("host=%q want=%q", got, want)
	}
	if got, want := windowsActivationServiceStartNames("host", true, true, true), []string{"PaperboatSshd", "PaperboatHostd", "PaperboatUpdated"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("host start order=%q want=%q", got, want)
	}
	if got, want := windowsActivationServiceStartNames("client", true, true, true), []string{"PaperboatHostd", "PaperboatUpdated"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("client start order=%q want=%q", got, want)
	}
	if got, want := windowsActivationServiceStartNames("host", false, true, true), []string{"PaperboatSshd", "PaperboatUpdated"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered host start order=%q want=%q", got, want)
	}
	sshArguments := []string{"daemon", "__windows-sshd-service", "--sshd", `C:\Program Files\OpenSSH\sshd.exe`, "--config", `C:\ProgramData\Paperboat\ssh\sshd_config`}
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
	journal.NewSSH.Arguments[3] = `C:\Temp\sshd.exe`
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

type busyWindowsActivationBackend struct {
	recordingWindowsActivationBackend
	startedHostd, startedSSH bool
}

func (b *busyWindowsActivationBackend) Drain(context.Context, windowsActivationJournal) error {
	b.events = append(b.events, "drain")
	return &autoupdate.ActiveTerminalSessionsError{RequiredVersion: testWindowsActivationJournal().Version}
}
func (b *busyWindowsActivationBackend) StartServices(ctx context.Context, h, u, ssh, local bool) error {
	b.startedHostd, b.startedSSH = h, ssh
	return b.recordingWindowsActivationBackend.StartServices(ctx, h, u, ssh, local)
}
func TestWindowsBusyActivationPreservesOriginalProcesses(t *testing.T) {
	b := &busyWindowsActivationBackend{}
	journal := testWindowsActivationJournal()
	journal.LocalDaemonWasRunning = true
	result, err := executeWindowsActivation(context.Background(), b, journal)
	var busy *autoupdate.ActiveTerminalSessionsError
	if !errors.As(err, &busy) || result.Stage != windowsActivationRolledBack {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	for _, event := range []string{"stop", "activate", "restore", "quarantine", "verifyRollback"} {
		if slices.Contains(b.events, event) {
			t.Fatalf("busy activation performed %s: %v", event, b.events)
		}
	}
	if b.startedHostd || b.startedSSH || b.startedLocalDaemon {
		t.Fatal("busy compensation restarted an original process")
	}
}

func TestWindowsBusyCompensationRecoveryAndFailure(t *testing.T) {
	for _, stage := range []windowsActivationStage{windowsActivationRollingBack, windowsActivationBusyReady} {
		t.Run(string(stage), func(t *testing.T) {
			j := testWindowsActivationJournal()
			j.Stage = stage
			j.PreDrainRollback = true
			j.BlockedReason = autoupdate.BlockedActiveTerminalSessions
			j.BlockedRetryAt = time.Now().Add(autoupdate.DefaultRetryFloor)
			b := &busyWindowsActivationBackend{}
			result, err := executeWindowsActivation(context.Background(), b, j)
			var busy *autoupdate.ActiveTerminalSessionsError
			if !errors.As(err, &busy) || result.Stage != windowsActivationRolledBack || b.startedHostd || b.startedSSH || b.startedLocalDaemon || slices.Contains(b.events, "quarantine") {
				t.Fatalf("recovery=%+v error=%v events=%v", result, err, b.events)
			}
		})
	}
	for _, failure := range []string{"candidate_stop", "journal:busy_ready", "start", "journal:rolled_back"} {
		t.Run(failure, func(t *testing.T) {
			b := &busyWindowsActivationBackend{recordingWindowsActivationBackend: recordingWindowsActivationBackend{fail: failure}}
			_, err := executeWindowsActivation(context.Background(), b, testWindowsActivationJournal())
			var busy *autoupdate.ActiveTerminalSessionsError
			if err == nil || errors.As(err, &busy) {
				t.Fatalf("cleanup failure reported expected busy: %v", err)
			}
			if b.startedHostd || b.startedSSH || b.startedLocalDaemon || slices.Contains(b.events, "quarantine") {
				t.Fatalf("cleanup touched original services: %v", b.events)
			}
		})
	}
}

func TestWindowsInterruptedDrainPreservesOriginalProcesses(t *testing.T) {
	for _, fail := range []string{"", "rollbackDrain"} {
		t.Run(fail, func(t *testing.T) {
			b := &recordingWindowsActivationBackend{fail: fail}
			j := testWindowsActivationJournal()
			j.Stage = windowsActivationDraining
			j.LocalDaemonWasRunning = true
			result, err := executeWindowsActivation(context.Background(), b, j)
			if err == nil {
				t.Fatal("interrupted activation must be reported")
			}
			if slices.Contains(b.events, "stop") || slices.Contains(b.events, "restore") || b.startedLocalDaemon {
				t.Fatalf("touched original processes: %v", b.events)
			}
			if !slices.Contains(b.events, "rollbackDrain") {
				t.Fatalf("missing admission rollback: %v", b.events)
			}
			if fail != "" {
				if result.Stage != windowsActivationDraining || slices.Contains(b.events, "candidate_stop") {
					t.Fatalf("failed admission rollback advanced: %v", b.events)
				}
			} else if result.Stage != windowsActivationRolledBack {
				t.Fatalf("stage=%s", result.Stage)
			}
		})
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

func TestWindowsDrainResponseLossDoesNotStopOriginalProcesses(t *testing.T) {
	b := &recordingWindowsActivationBackend{fail: "drain"}
	j := testWindowsActivationJournal()
	j.LocalDaemonWasRunning = true
	result, err := executeWindowsActivation(context.Background(), b, j)
	if err == nil || result.Stage != windowsActivationRolledBack {
		t.Fatalf("stage=%s err=%v", result.Stage, err)
	}
	if slices.Contains(b.events, "stop") || slices.Contains(b.events, "restore") || b.startedLocalDaemon {
		t.Fatalf("ambiguous drain touched live processes: %v", b.events)
	}
	if !slices.Contains(b.events, "rollbackDrain") {
		t.Fatalf("did not reconcile exact admission: %v", b.events)
	}
}

type consecutiveWindowsBackend struct {
	recordingWindowsActivationBackend
	held string
}

func (b *consecutiveWindowsBackend) Drain(_ context.Context, j windowsActivationJournal) error {
	if b.held != "" && b.held != j.TransactionID {
		return errors.New("admission remains held")
	}
	b.held = j.TransactionID
	return b.event("drain")
}
func (b *consecutiveWindowsBackend) CommitGate(_ context.Context, j windowsActivationJournal) error {
	if b.held != "" && b.held != j.TransactionID {
		return errors.New("wrong admission owner")
	}
	if err := b.event("gate_commit"); err != nil {
		return err
	}
	b.held = ""
	return nil
}
func TestWindowsSuccessiveUpdatesReleaseAdmission(t *testing.T) {
	b := &consecutiveWindowsBackend{}
	j := testWindowsActivationJournal()
	for _, id := range []string{strings.Repeat("1", 32), strings.Repeat("2", 32)} {
		j.TransactionID = id
		result, err := executeWindowsActivation(context.Background(), b, j)
		if err != nil || result.Stage != windowsActivationCommitted || b.held != "" {
			t.Fatalf("stage=%s held=%s err=%v events=%v", result.Stage, b.held, err, b.events)
		}
	}
}

func (b *recordingWindowsActivationBackend) CommitGate(context.Context, windowsActivationJournal) error {
	return b.event("gate_commit")
}

func TestWindowsCommitFailureResumesWithoutRollback(t *testing.T) {
	for _, fail := range []string{"gate_commit", "journal:committed"} {
		t.Run(fail, func(t *testing.T) {
			b := &consecutiveWindowsBackend{}
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
			if err != nil || result.Stage != windowsActivationCommitted || b.held != "" || slices.Contains(b.events, "stop") {
				t.Fatalf("resume=%s err=%v events=%v", result.Stage, err, b.events)
			}
		})
	}
}
