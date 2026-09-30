//go:build windows

package updated

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestWindowsAutomaticUpdatesDisabledDoesNotResolve(t *testing.T) {
	calls := 0
	c := &windowsController{activeVersion: "local-dev", config: WindowsConfig{AutomaticChecks: false}, resolve: func(context.Context) (workerupdate.Release, bool, error) {
		calls++
		return workerupdate.Release{}, false, nil
	}}
	got, err := c.checkRelease(context.Background())
	if err != nil || got.Version != "local-dev" || calls != 0 {
		t.Fatalf("disabled automatic request: %+v calls=%d err=%v", got, calls, err)
	}
}

func TestWindowsLocalBaselineJournalRemainsDistinctFromCandidate(t *testing.T) {
	j := testWindowsActivationJournal()
	s := installsource.Source{Version: "local-dev", Platform: "windows", Architecture: j.Architecture, SHA256: j.PreviousBinary.SHA256, Length: j.PreviousBinary.Length, Distribution: installsource.Custom}
	j.PreviousVersion = s.Version
	j.PreviousSource = &s
	if !validWindowsActivationJournal(j) {
		t.Fatal("local installed baseline rejected")
	}
	j.PreviousSource.SHA256 = j.ManifestSHA256
	if validWindowsActivationJournal(j) {
		t.Fatal("mismatched local baseline accepted")
	}
}

func TestWindowsInstalledVersionComparisonPreservesOfficialRollbackProtection(t *testing.T) {
	j := testWindowsActivationJournal()
	s := installsource.Source{Version: "2026.09.19.1", Platform: "windows", Architecture: j.Architecture, SHA256: j.PreviousBinary.SHA256, Length: j.PreviousBinary.Length, Distribution: installsource.Official, AutomaticUpdates: true}
	if got, err := compareWindowsInstalledVersion("2026.09.18.0", s.Version, s); err != nil || got >= 0 {
		t.Fatalf("official downgrade comparison=%d err=%v", got, err)
	}
	s.Distribution = installsource.Custom
	s.AutomaticUpdates = false
	s.Version = "local-dev"
	if got, err := compareWindowsInstalledVersion("2026.09.18.0", s.Version, s); err != nil || got <= 0 {
		t.Fatalf("custom adoption comparison=%d err=%v", got, err)
	}
}

func TestWindowsBackgroundCheckOnlyReportsAvailableVersion(t *testing.T) {
	c := &windowsController{activeVersion: "2026.08.22.1", config: WindowsConfig{AutomaticChecks: true}, handoff: make(chan struct{}), resolve: func(context.Context) (workerupdate.Release, bool, error) {
		return workerupdate.Release{Version: "2026.08.23.1"}, true, nil
	}}
	got, err := c.checkRelease(context.Background())
	if err != nil || got.Updated || got.Version != "2026.08.23.1" || activationRequested(c.handoff) {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}
