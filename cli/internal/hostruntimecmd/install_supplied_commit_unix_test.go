//go:build darwin || linux

package hostruntimecmd

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

// A .pb-update bundle is installed with an empty source in the request: its real identity is
// only read from the installed binary afterwards. The commit request must carry that identity,
// otherwise the privileged commit verifies an empty source and an installation that landed is
// reported as 'installation commit could not be confirmed'.
func TestSuppliedCommitRequestCarriesTheInstalledBundleIdentity(t *testing.T) {
	request := hostinstall.Request{Schema: hostinstall.SchemaV1, UID: 1000, UpdateBundle: "/tmp/paperboat.pb-update", SuppliedUpdate: true}
	// Inspecting an installed executable reports it as a custom build with no automatic
	// updates; the install step journaled the bundle's source as official with them.
	installed := installsource.Source{Version: "2026.10.19.4", SHA256: "8473fa44da589a5b0ce1e1beab4b520031d64c9d3a5439b77da3a50ca8125535", Length: 93000000, Platform: "linux", Architecture: "amd64", Distribution: installsource.Custom}
	journaled := installed
	journaled.Distribution, journaled.AutomaticUpdates = installsource.Official, true
	got := suppliedCommitRequest(request, installed)
	if got.Source != journaled {
		t.Fatalf("commit request source = %+v, want the journaled %+v", got.Source, journaled)
	}
	if got.UpdateBundle != request.UpdateBundle || got.UID != request.UID || !got.SuppliedUpdate {
		t.Fatalf("commit request changed other fields: %+v", got)
	}
	if request.Source != (installsource.Source{}) {
		t.Fatal("the original request was modified")
	}
}
