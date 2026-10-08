package updated

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

type provenanceWindowsBackend struct {
	featureWindowsBackend
	committed []windowsActivationJournal
}

func (b *provenanceWindowsBackend) CommitCLI(ctx context.Context, j windowsActivationJournal) error {
	b.committed = append(b.committed, j)
	return b.recordingWindowsActivationBackend.CommitCLI(ctx, j)
}

func TestWindowsRollbackCarriesPreviousSourceAndByteIdentityToCommit(t *testing.T) {
	for _, maintenance := range []bool{false, true} {
		name := "ordinary"
		if maintenance {
			name = "maintenance"
		}
		t.Run(name, func(t *testing.T) {
			j := testWindowsFeatureJournal()
			j.Release.SupervisorMaintenance = maintenance
			previous := installsource.Source{Version: j.PreviousVersion, Platform: "windows", Architecture: j.Architecture, SHA256: j.PreviousBinary.SHA256, Length: j.PreviousBinary.Length, Distribution: installsource.Custom}
			j.PreviousSource = &previous
			bindWindowsTestCandidate(&j)
			b := &provenanceWindowsBackend{}
			b.fail = "feature:health"
			if maintenance {
				b.fail = "health"
			}
			result, err := executeWindowsActivation(context.Background(), b, j)
			if err == nil || result.Stage != windowsActivationRolledBack || len(b.committed) != 1 {
				t.Fatalf("rollback stage=%s commits=%d error=%v", result.Stage, len(b.committed), err)
			}
			got := b.committed[0]
			if got.Stage != windowsActivationRollingBack || got.PreviousSource == nil || *got.PreviousSource != previous || got.PreviousBinary != j.PreviousBinary || got.PreviousVersion != j.PreviousVersion || got.Version != j.Version || got.OldUpdater.Executable != j.OldUpdater.Executable || got.OldUpdater.SHA256 != j.OldUpdater.SHA256 || got.OldHostd.Executable != j.OldHostd.Executable {
				t.Fatal("rollback discarded previous provenance, byte identity or native pins")
			}
		})
	}
}
