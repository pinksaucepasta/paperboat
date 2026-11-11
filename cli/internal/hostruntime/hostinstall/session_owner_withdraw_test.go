//go:build linux || darwin || windows

package hostinstall

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

type orderedGuardUndo struct{ note func(string) }

func (u orderedGuardUndo) Restore(context.Context) error { u.note("guard restored"); return nil }
func (u orderedGuardUndo) Discard()                      { u.note("guard kept") }

// victus 2026-11-10: a Windows update that failed after its services had started rolled back to the previous
// version, but the candidate's session owner and SSH service stayed and the machine guard stayed on the
// candidate's version. A failed replacement withdraws its candidate and puts the guard back, after the lifecycle
// restored the previous version and even when the caller's context is gone; a committed one keeps both.
func TestAFailedReplacementWithdrawsItsCandidateAndRestoresTheGuard(t *testing.T) {
	notReady := errors.New("the new version did not become ready")
	noGuard := errors.New("install Paperboat machine guard")
	leftover := errors.New("session owner still registered")
	for _, test := range []struct {
		name                              string
		guardErr, replaceErr, withdrawErr error
		want                              string
	}{
		{name: "failed", replaceErr: notReady, want: "journal,guard installed,replace,rollback,withdraw,guard restored"},
		{name: "failed, withdraw fails", replaceErr: notReady, withdrawErr: leftover, want: "journal,guard installed,replace,rollback,withdraw,guard restored"},
		{name: "guard not installed", guardErr: noGuard, want: "journal,rollback,withdraw"},
		{name: "committed", want: "journal,guard installed,replace,guard kept"},
	} {
		var events []string
		note := func(event string) { events = append(events, event) }
		ctx, cancel := context.WithCancel(context.Background())
		lifecycle := func(ctx context.Context, body func(context.Context) error) error {
			note("journal")
			if err := body(ctx); err != nil {
				note("rollback")
				return err
			}
			return nil
		}
		installGuard := func(context.Context) (guardUndo, error) {
			if test.guardErr != nil {
				return nil, test.guardErr
			}
			note("guard installed")
			return orderedGuardUndo{note}, nil
		}
		replace := func(context.Context) error {
			note("replace")
			if test.replaceErr != nil {
				cancel() // the caller's deadline ended the attempt
			}
			return test.replaceErr
		}
		withdraw := func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Errorf("%s: the candidate is withdrawn under the caller's canceled context", test.name)
			}
			note("withdraw")
			return test.withdrawErr
		}
		err := replaceCandidate(ctx, lifecycle, installGuard, replace, withdraw)
		cancel()
		if got := strings.Join(events, ","); got != test.want {
			t.Errorf("%s: steps %q, want %q", test.name, got, test.want)
		}
		for _, cause := range []error{test.guardErr, test.replaceErr, test.withdrawErr} {
			if cause != nil && !errors.Is(err, cause) {
				t.Errorf("%s: %v is not reported in %v", test.name, cause, err)
			}
		}
		if test.guardErr == nil && test.replaceErr == nil && err != nil {
			t.Errorf("%s: %v", test.name, err)
		}
	}
}

// An attempt that died before publishing its session owner left no descriptor: withdrawing it is a no-op, not a failure
// that blocks every later install.
func TestWithdrawingACandidateThatNeverPublishedSucceeds(t *testing.T) {
	candidate := candidateSessionOwner{catalog: sessionowner.Catalog{Root: t.TempDir()}, generation: strings.Repeat("ab", 16)}
	if err := candidate.withdrawUnless(context.Background(), strings.Repeat("cd", 16)); err != nil {
		t.Fatalf("a candidate with no descriptor: %v", err)
	}
}

// Only the session owner the attempt itself published is withdrawn: one published before the attempt (an earlier
// version's archive, a reinstall of the same build) and the one the installation record names stay.
func TestOnlyTheAttemptsOwnSessionOwnerIsWithdrawn(t *testing.T) {
	generation := strings.Repeat("ab", 16)
	ctx := context.Background()
	// These candidates have no catalogue, so reaching it is an error: nil proves they were kept.
	for name, kept := range map[string]error{
		"published before the attempt":     candidateSessionOwner{generation: generation, existed: true}.withdrawUnless(ctx, ""),
		"named by the installation record": candidateSessionOwner{generation: generation}.withdrawUnless(ctx, generation),
		"no candidate":                     candidateSessionOwner{}.withdrawUnless(ctx, generation),
	} {
		if kept != nil {
			t.Errorf("%s: withdrawn: %v", name, kept)
		}
	}
	for name, committed := range map[string]string{"previous version restored": strings.Repeat("cd", 16), "first installation": ""} {
		if err := (candidateSessionOwner{generation: generation}).withdrawUnless(ctx, committed); err == nil {
			t.Errorf("%s: the attempt's own session owner was kept", name)
		}
	}
}
