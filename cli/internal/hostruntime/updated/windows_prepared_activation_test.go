package updated

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestWindowsPreparedUsesOriginalSignedPolicyAndNativeOwner(t *testing.T) {
	for _, maintenance := range []bool{false, true} {
		j := testWindowsFeatureJournal()
		j.Release.SupervisorMaintenance = maintenance
		bindWindowsTestCandidate(&j)
		original := j.Release
		candidate := j.Candidate
		activated := false
		result, err := executeWindowsPreparedActivation(t.Context(), j, func(_ context.Context, current *windowsActivationJournal) error {
			if !reflect.DeepEqual(current.Release, original) || current.Candidate != candidate {
				t.Fatal("native activation received changed signed policy or approval identity")
			}
			activated = true
			return nil
		}, func(receipt windowsActivationJournal) error {
			if !activated || receipt.Stage != windowsActivationCommitted {
				t.Fatal("receipt committed before native owner completed")
			}
			return nil
		})
		if err != nil || result.Stage != windowsActivationCommitted {
			t.Fatal("native activation did not commit receipt")
		}
	}
}

func TestWindowsPreparedRejectsUnapprovedOrChangedPolicyBeforeNativeMutation(t *testing.T) {
	for _, kind := range []string{"unapproved", "approval", "policy"} {
		t.Run(kind, func(t *testing.T) {
			j := testWindowsActivationJournal()
			switch kind {
			case "unapproved":
				j.Stage = windowsActivationAwaitingApproval
				j.ApprovedCandidateID = ""
			case "approval":
				j.ApprovedCandidateID = "different"
			case "policy":
				j.Release.DrainTimeout++
			}
			mutations := 0
			_, err := executeWindowsPreparedActivation(t.Context(), j, func(context.Context, *windowsActivationJournal) error { mutations++; return nil }, func(windowsActivationJournal) error { mutations++; return nil })
			if err == nil || mutations != 0 {
				t.Fatal("invalid approval or policy reached native mutation")
			}
		})
	}
}

func TestWindowsPreparedRetainsCommitRecoveryWithoutIndependentRollback(t *testing.T) {
	j := testWindowsActivationJournal()
	failure := workerupdate.ErrActivationGate
	writes := 0
	result, err := executeWindowsPreparedActivation(t.Context(), j, func(_ context.Context, receipt *windowsActivationJournal) error {
		receipt.Stage = windowsActivationCommitReady
		return failure
	}, func(windowsActivationJournal) error { writes++; return nil })
	if !errors.Is(err, failure) || result.Stage != windowsActivationCommitReady || writes != 0 {
		t.Fatal("signed gate failure lost native committed recovery state")
	}
	result, err = executeWindowsPreparedActivation(t.Context(), result, func(context.Context, *windowsActivationJournal) error { return nil }, func(receipt windowsActivationJournal) error { writes++; return nil })
	if err != nil || result.Stage != windowsActivationCommitted || writes != 1 {
		t.Fatal("native committed retry did not finish receipt")
	}
}

func TestWindowsPendingSourceNeverPromotesCandidateBytes(t *testing.T) {
	old := installsource.Source{Version: "old", Distribution: installsource.Custom}
	candidate := installsource.Source{Version: "candidate", Distribution: installsource.Official}
	for _, test := range []struct {
		committed bool
		baseline  *installsource.Source
		want      installsource.Source
	}{
		{true, &old, candidate},
		{false, &old, old},
		{false, nil, installsource.Source{}},
	} {
		got := windowsCommittedSourceProjection(candidate, test.committed, test.baseline)
		if got != test.want {
			t.Fatal("runtime candidate escaped into committed-source status")
		}
	}
}
