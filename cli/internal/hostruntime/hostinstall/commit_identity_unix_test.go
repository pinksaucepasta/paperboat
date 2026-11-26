//go:build darwin || linux

package hostinstall

import (
	"runtime"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
)

// Canaries 2026-11-26: a paired bundle commit over a record with automatic updates off
// reported "the recorded installation is not the executable being committed", because the
// client sends the policy on while the record keeps it off. The artifact must still match exactly.
func TestCommitComparesTheArtifactNotTheUpdatePolicy(t *testing.T) {
	recorded := installsource.Source{Version: "2026.10.20.7", Platform: runtime.GOOS, Architecture: runtime.GOARCH, SHA256: strings.Repeat("a", 64), Length: 1, Distribution: installsource.Official}
	requested := recorded
	requested.AutomaticUpdates = true
	if !sameCommittedArtifact(recorded, requested) {
		t.Fatal("a differing update policy alone refused the commit")
	}
	for name, change := range map[string]func(*installsource.Source){
		"digest":       func(s *installsource.Source) { s.SHA256 = strings.Repeat("b", 64) },
		"length":       func(s *installsource.Source) { s.Length = 2 },
		"version":      func(s *installsource.Source) { s.Version = "2026.10.20.8" },
		"distribution": func(s *installsource.Source) { s.Distribution, s.AutomaticUpdates = installsource.Custom, false },
	} {
		other := requested
		change(&other)
		if sameCommittedArtifact(recorded, other) {
			t.Fatalf("%s: a different artifact was accepted", name)
		}
	}
}

// testSource is a valid custom source; commit checks compare only valid artifacts.
func testSource(version, digit string) installsource.Source {
	return installsource.Source{Version: version, Platform: runtime.GOOS, Architecture: runtime.GOARCH, SHA256: strings.Repeat(digit, 64), Length: 1, Distribution: installsource.Custom}
}
