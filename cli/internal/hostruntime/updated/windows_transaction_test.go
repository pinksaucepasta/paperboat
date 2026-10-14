package updated

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"strings"
	"testing"
	"time"
)

func testWindowsActivationJournal() windowsActivationJournal {
	c := windowsActivationComponent{Path: `C:\Paperboat\candidate.exe`, SHA256: strings.Repeat("a", 64), Length: 1}
	previous := c
	previous.Path = `C:\Program Files\Paperboat\bin\pb.exe`
	j := windowsActivationJournal{Candidate: workerupdate.PreparedCandidate{ID: strings.Repeat("d", 64), Version: "2026.08.23.1", Platform: "windows", Architecture: "amd64", SHA256: c.SHA256, Length: c.Length}, ApprovedCandidateID: strings.Repeat("d", 64), Schema: windowsActivationJournalSchema, TransactionID: strings.Repeat("1", 32), PreviousVersion: "2026.08.22.1", Version: "2026.08.23.1", Architecture: "amd64", Stage: windowsActivationStaged, Runtime: c, CLI: c, Hostd: c, Updater: c, PreviousBinary: previous, OldHostd: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-hostd", "--instance", "u0123456789abcdef01234567"}, WasRunning: true}, NewHostd: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-hostd", "--instance", "u0123456789abcdef01234567"}}, OldUpdater: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-updated", "--instance", "u0123456789abcdef01234567"}, WasRunning: true}, NewUpdater: windowsServiceTarget{Executable: `C:\Program Files\Paperboat\bin\pb.exe`, Arguments: []string{"daemon", "__runtime-updated", "--instance", "u0123456789abcdef01234567"}}, Release: workerupdate.Release{SupervisorMaintenance: true, Version: "2026.08.23.1", Platform: "windows", Architecture: "amd64", SHA256: c.SHA256, Length: c.Length, ManifestSHA256: strings.Repeat("b", 64), CanaryPath: "/_paperboat/update-canary", CanaryStatus: 204, CanarySamples: 3, CanaryTimeout: time.Second, DrainTimeout: time.Second, StabilityWindow: time.Second, StabilityInterval: time.Second, RollbackTimeout: time.Second, HostdAPIMin: 1, HostdAPIMax: 2, RuntimeAPIMin: 1, RuntimeAPIMax: 2}}
	bindWindowsTestCandidate(&j)
	return j
}

func TestWindowsActivationFailureIsBoundedAndSingleLine(t *testing.T) {
	message := boundedWindowsActivationFailure(errors.New(strings.Repeat("x", 3000) + "\r\nsecret"))
	if len(message) != 2048 || strings.ContainsAny(message, "\r\n\x00") {
		t.Fatalf("failure length=%d value=%q", len(message), message)
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
	if !windowsActivationNeedsResume(journal, journal.Version, false) {
		t.Fatal("committed candidate failed to finish its remaining signed receipt")
	}
	if windowsActivationNeedsResume(journal, journal.Version, true) {
		t.Fatal("candidate stole a live activator's receipt")
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

func TestWindowsActivationJournalPersistsForceSeparatelyFromManualApproval(t *testing.T) {
	for _, test := range []struct {
		name           string
		manualApproval bool
		force          bool
	}{
		{name: "ordinary approval", manualApproval: true},
		{name: "explicit force", manualApproval: true, force: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal := testWindowsActivationJournal()
			journal.ManualApproval = test.manualApproval
			journal.Force = test.force
			encoded, err := json.Marshal(journal)
			if err != nil {
				t.Fatal(err)
			}
			var decoded windowsActivationJournal
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.ManualApproval != test.manualApproval || decoded.Force != test.force {
				t.Fatalf("decoded approval=%t force=%t", decoded.ManualApproval, decoded.Force)
			}
		})
	}
}

func testWindowsFeatureJournal() windowsActivationJournal {
	j := testWindowsActivationJournal()
	j.Release.SupervisorMaintenance = false
	bindWindowsTestCandidate(&j)
	j.PreviousRuntime = j.PreviousBinary
	j.PreviousRuntime.Path = `C:\Paperboat\versions\2026.08.22.1\pb.exe`
	return j
}

func bindWindowsTestCandidate(j *windowsActivationJournal) {
	raw, _ := json.Marshal(j.Release)
	digest := sha256.Sum256(raw)
	j.Candidate.ID = hex.EncodeToString(digest[:])
	j.Candidate.OwnerMaintenance = j.Release.SupervisorMaintenance
	j.ApprovedCandidateID = j.Candidate.ID
}
