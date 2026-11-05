//go:build darwin

package runtime

import "testing"

// hostd must not own the system proxy: its configurator is the guard hand-off, never the networksetup manager.
func TestHostdPrivatePACIsTheGuardHandoffNotNetworksetup(t *testing.T) {
	pac, err := newPlatformPrivatePAC(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pac.(guardPAC); !ok {
		t.Fatalf("hostd configurator is %T, want guardPAC", pac)
	}
}
