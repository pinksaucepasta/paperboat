//go:build linux || darwin || windows

package hostinstall

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
)

func TestBundleKeepsTheAutomaticUpdatePreferenceOfWhatItReplaces(t *testing.T) {
	candidate := installsource.Source{Version: "2026.10.20.5", Distribution: installsource.Official, AutomaticUpdates: true}
	for _, previousOn := range []bool{false, true} {
		previous := installsource.Source{Version: "2026.10.20.3", Distribution: installsource.Official, AutomaticUpdates: previousOn}
		for _, pending := range []bool{false, true} {
			got, err := ReplaceBundleSource(previous, pending, candidate)
			if err != nil || got.AutomaticUpdates != previousOn || got.Version != candidate.Version {
				t.Fatalf("previous automatic=%v pending=%v: %+v %v", previousOn, pending, got, err)
			}
		}
	}
}

func TestPendingInstallIsNeverReplacedByAnOlderSignedRelease(t *testing.T) {
	previous := installsource.Source{Version: "2026.10.20.8", Distribution: installsource.Official, AutomaticUpdates: true}
	for name, tc := range map[string]struct {
		candidate string
		pending   bool
		refused   bool
	}{
		"older over pending":  {"2026.10.20.7", true, true},
		"older, enrolled":     {"2026.10.20.7", false, false},
		"same over pending":   {"2026.10.20.8", true, false},
		"newer over pending":  {"2026.10.20.9", true, false},
		"older day, pending":  {"2026.09.30.99", true, true},
		"unparsable, pending": {"dev-abc", true, true},
	} {
		_, err := ReplaceBundleSource(previous, tc.pending, installsource.Source{Version: tc.candidate, Distribution: installsource.Official})
		if tc.refused != (err != nil) || tc.refused && !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s: refused=%v err=%v", name, tc.refused, err)
		}
		if tc.refused {
			if message, ok := FailureMessage(err); !ok || !strings.Contains(message, "older than the installed") {
				t.Fatalf("%s: message %q", name, message)
			}
		}
	}
	// A custom pending build has no release order to protect: the official bundle may replace it.
	custom := installsource.Source{Version: "dev-1", Distribution: installsource.Custom}
	if _, err := ReplaceBundleSource(custom, true, installsource.Source{Version: "2026.10.20.7", Distribution: installsource.Official}); err != nil {
		t.Fatalf("an official bundle over a custom pending build was refused: %v", err)
	}
}
