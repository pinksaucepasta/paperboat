//go:build darwin || linux

package hostinstall

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

// F14-08: when the final commit of an update is refused, the helper says which check failed instead of
// the generic "host runtime command failed".
func TestCommitRefusalsNameTheCheckThatFailed(t *testing.T) {
	pending := installsource.Source{Version: "pending"}
	request := Request{Schema: SchemaV1, Source: pending}
	for name, tc := range map[string]struct {
		journal installJournal
		request Request
		want    string
	}{
		"services not started": {installJournal{Stage: "binaries_activated", Source: pending}, request, "has not started its services"},
		"different executable": {installJournal{Stage: "services_started", Source: pending}, Request{Schema: SchemaV1, Source: installsource.Source{Version: "other"}}, "different executable"},
	} {
		err := checkPendingInstallation(tc.journal, tc.request)
		message, ok := FailureMessage(err)
		if !errors.Is(err, ErrInvalidRequest) || !ok || !strings.Contains(message, tc.want) {
			t.Fatalf("%s: %v (%q %v)", name, err, message, ok)
		}
	}
	if err := checkPendingInstallation(installJournal{Stage: "services_started", Source: pending}, request); err != nil {
		t.Fatalf("a matching pending installation was refused: %v", err)
	}
}
