package hostinstall

import (
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
)

// A pb update --from of the installed signed bytes changes only labels; the carrier compare
// (SameArtifact) must not restage the generation it would share a socket with
// (smoke 1126: "hostd.sock.c<gen>: no such file", rollback).
func TestCarrierCompareIgnoresInstallLabels(t *testing.T) {
	signed := installsource.Source{Version: "2026.11.26.21", Platform: "linux", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Length: 7, Distribution: installsource.Official, AutomaticUpdates: true}
	from := signed
	from.Distribution, from.AutomaticUpdates = installsource.Custom, false
	if signed.Validate() != nil || !signed.SameArtifact(from) {
		t.Fatal("same bytes with other labels restage the carrier")
	}
	other := signed
	other.SHA256 = strings.Repeat("b", 64)
	if signed.SameArtifact(other) {
		t.Fatal("other bytes do not restage the carrier")
	}
}
