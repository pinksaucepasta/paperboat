//go:build darwin || linux

package hostinstall

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
)

// A fresh machine guard learns the local access domain from the server the install
// request names, so an unenrolled supplied install must not drop it.
func TestUnboundSuppliedRequestKeepsTheControlURL(t *testing.T) {
	request := unboundSuppliedRequest(Request{Schema: SchemaV1, Platform: "linux", User: "tester", UID: 1000, GID: 1000, Group: "tester", Home: "/home/tester", Executable: "/home/tester/pb", ControlURL: "https://api.example.test", Force: true})
	if request.ControlURL != "https://api.example.test" {
		t.Fatalf("control URL = %q; a fresh machine guard could not read its local access domain", request.ControlURL)
	}
	if !request.EnrollmentPending || request.UID != 1000 || request.Executable != "/home/tester/pb" || request.Force {
		t.Fatalf("unbound request changed shape: %+v", request)
	}
}

// `pb update` on an install whose enrollment is pending installs the signed bundle through the supplied-install
// transaction. The pending record is re-pointed at the new slot and stays pending, so the enrollment commit that follows
// accepts the new binary (journal source == request source) and refuses a stale commit of the old one.
func TestUpdatingAPendingInstallKeepsItPendingAtTheNewSlotAndTheCommitVerifiesIt(t *testing.T) {
	oldSource, newSource := installsource.Source{Version: "old"}, installsource.Source{Version: "new"}
	update := unboundSuppliedRequest(Request{Schema: SchemaV1, Platform: "linux", User: "tester", UID: 1000, GID: 1000, Group: "tester", Home: "/home/tester", Executable: "/pb", Source: newSource})
	if !update.EnrollmentPending || update.Source != newSource {
		t.Fatalf("update of a pending install = %+v", update)
	}
	journal := installJournal{Stage: "services_started", Source: update.Source}
	if err := checkPendingInstallation(journal, Request{Schema: SchemaV1, Source: newSource}); err != nil {
		t.Fatalf("the enrollment commit refused the updated slot: %v", err)
	}
	if err := checkPendingInstallation(journal, Request{Schema: SchemaV1, Source: oldSource}); err == nil {
		t.Fatal("a stale enrollment commit of the replaced binary was accepted")
	}
}
