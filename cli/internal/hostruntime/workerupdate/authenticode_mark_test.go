package workerupdate

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseindex"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releasepolicy"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

// The signed index's Authenticode mark must survive every hop to the native verifier: index -> release ->
// persisted journal -> release again (a restart between download and activation) -> supervisor components.
func TestAuthenticodeMarkTravelsFromTheSignedIndexToTheVerifier(t *testing.T) {
	for _, marked := range []bool{false, true} {
		index := releaseindex.Index{Version: "2026.11.09.1", DeploymentPlan: &releasepolicy.Plan{},
			Targets: []releaseindex.Target{{Component: "pb", SHA256: "abc", Length: 5, Platform: "windows", Architecture: "amd64", AuthenticodeSigned: marked}}}
		release, ok := releaseFromIndex(index)
		if !ok || release.AuthenticodeSigned != marked || release.Hostd.AuthenticodeSigned != marked || release.Updater.AuthenticodeSigned != marked || release.Launcher.AuthenticodeSigned != marked {
			t.Fatalf("marked=%v: release = %#v", marked, release)
		}
		journal := withRelease(updateflow.Journal{}, release, "/staged/pb.exe")
		if journal.CandidateAuthenticodeSigned != marked || releaseFromJournal(journal).AuthenticodeSigned != marked {
			t.Fatalf("marked=%v: journal lost the mark: %#v", marked, journal)
		}
		completed, err := completeJournalRelease(releaseFromJournal(journal))
		if err == nil && completed.Hostd.AuthenticodeSigned != marked {
			t.Fatalf("marked=%v: completed release lost the mark", marked)
		}
	}
}
