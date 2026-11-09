package updated

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// An installed update whose commit never finished (commit_failed) needs attention, so doctor must not say healthy;
// a finish that is still being retried, and a merely quarantined candidate, are not faults yet.
func TestUpdateHealthReportsAnUpdateWhoseCommitNeverFinished(t *testing.T) {
	for _, test := range []struct {
		name        string
		transaction workerupdate.TransactionState
		want        string
	}{
		{"commit failed", workerupdate.TransactionState{Failure: updateflow.FailureCommit, Quarantined: true}, "recovery_required"},
		{"release revoked", workerupdate.TransactionState{Failure: updateflow.FailureRevoked, Quarantined: true}, "recovery_required"},
		{"rollback failed", workerupdate.TransactionState{Failure: updateflow.FailureRollback}, "recovery_required"},
		{"commit being retried", workerupdate.TransactionState{CommitPending: true}, "healthy"},
		{"candidate quarantined", workerupdate.TransactionState{Failure: updateflow.FailureHealth, Quarantined: true}, "healthy"},
	} {
		if got := (ControlResponse{Status: "ok", Transaction: test.transaction}).UpdateHealth(); got != test.want {
			t.Errorf("%s: health %q, want %q", test.name, got, test.want)
		}
	}
}
