package configsync

import (
	"errors"
	"testing"
)

type snapshotPrivateCause struct{}

func (snapshotPrivateCause) Error() string { return "private snapshot path /Users/alice/.secret" }

func TestSnapshotFailurePreservesCauseWithoutFormattingIt(t *testing.T) {
	cause := snapshotPrivateCause{}
	err := &snapshotFailure{cause: cause}
	var preserved snapshotPrivateCause
	if !errors.Is(err, ErrSnapshotInvalid) || !errors.Is(err, cause) || !errors.As(err, &preserved) {
		t.Fatalf("snapshot cause was not preserved: %v", err)
	}
	if got := err.Error(); got != ErrSnapshotInvalid.Error() {
		t.Fatalf("snapshot failure exposed cause text: %q", got)
	}
}

func TestChangedPathsIncludesAddModifyAndDelete(t *testing.T) {
	before := map[string]FileState{
		".deleted": {Hash: "old"},
		".same":    {Hash: "same"},
		".changed": {Hash: "old"},
	}
	after := map[string]FileState{
		".same":    {Hash: "same"},
		".changed": {Hash: "new"},
		".added":   {Hash: "new"},
	}
	got := ChangedPaths(before, after)
	want := []string{".added", ".changed", ".deleted"}
	if len(got) != len(want) {
		t.Fatalf("changed = %#v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("changed = %#v, want %#v", got, want)
		}
	}
}
