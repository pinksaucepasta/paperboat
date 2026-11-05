//go:build linux || darwin

package hostinstall

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// A carrier that is declared but not running (stopped, or exited and never restarted) is started by the updater's
// reconcile pass. Before, the pass only waited for it: 15 s of holding the update lock every 5 s, which kept the
// install commit out for minutes and left the machine without its network.
func TestReconcileStartsADeclaredCarrierThatIsNotRunning(t *testing.T) {
	for name, tc := range map[string]struct {
		status service.NativeControllerStatus
		want   ensureAction
	}{
		"serving":                    {service.NativeControllerStatus{Registered: true, Enabled: true, Running: true}, ensureNothing},
		"stopped but declared":       {service.NativeControllerStatus{Registered: true, Enabled: true}, ensureStart},
		"declared, boot start off":   {service.NativeControllerStatus{Registered: true}, ensureStart},
		"missing":                    {service.NativeControllerStatus{}, ensureInstall},
		"running without definition": {service.NativeControllerStatus{Running: true}, ensureNothing},
	} {
		if got := carrierEnsureAction(tc.status); got != tc.want {
			t.Errorf("%s: action %d, want %d", name, got, tc.want)
		}
	}
}
