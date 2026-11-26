//go:build linux || darwin || windows

package hostinstall

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
)

type failingGuardUndo struct{ err error }

func (u failingGuardUndo) Restore(context.Context) error { return u.err }
func (failingGuardUndo) Discard()                        {}

// A not-ready replacement whose guard or session-owner restore failed is not "restored": both failures are tagged so
// the privileged step never exits as "the previous version was restored" (review of 544557f43).
func TestReplaceCandidateTagsAnUnfinishedGuardOrOwnerRestore(t *testing.T) {
	restoreFailure := errors.New("restore failed")
	for _, test := range []struct {
		name               string
		guardErr, withdraw error
		incomplete         bool
	}{
		{name: "both restored"},
		{name: "guard not restored", guardErr: restoreFailure, incomplete: true},
		{name: "session owner not withdrawn", withdraw: restoreFailure, incomplete: true},
	} {
		lifecycle := func(ctx context.Context, replace func(context.Context) error) error { return replace(ctx) }
		installGuard := func(context.Context) (guardUndo, error) { return failingGuardUndo{test.guardErr}, nil }
		replace := func(context.Context) error { return UpdateNotReady(errors.New("carrier did not start")) }
		withdraw := func(context.Context) error { return test.withdraw }
		err := replaceCandidate(context.Background(), lifecycle, installGuard, replace, withdraw)
		if got := errors.Is(err, service.ErrLifecycleRollbackIncomplete); got != test.incomplete {
			t.Errorf("%s: incomplete=%v, want %v: %v", test.name, got, test.incomplete, err)
		}
		if got := RestoredNotReady(err); got == test.incomplete {
			t.Errorf("%s: RestoredNotReady=%v: %v", test.name, got, err)
		}
	}
}
