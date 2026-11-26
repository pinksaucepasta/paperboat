//go:build windows

package updated

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
)

// be11f83c5 on Windows: a custom build moved to a signed release kept automatic updates off.
func TestPreparedAutomaticUpdatesFollowsReplaceBundleSource(t *testing.T) {
	if !preparedAutomaticUpdates(installsource.Source{Distribution: installsource.Custom}) {
		t.Fatal("custom to signed release left automatic updates off")
	}
	if preparedAutomaticUpdates(installsource.Source{Distribution: installsource.Official}) {
		t.Fatal("signed installation lost the owner's automatic-updates off")
	}
}
