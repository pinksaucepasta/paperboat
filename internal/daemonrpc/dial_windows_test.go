//go:build windows

package daemonrpc

import "testing"

func TestDaemonPipeAddressUsesScopedDefault(t *testing.T) {
	got, err := daemonPipeAddress("")
	if err != nil {
		t.Fatal(err)
	}
	if want := DefaultSocketAddress(); got != want || got == DefaultWindowsNamedPipe {
		t.Fatalf("default address=%q want scoped %q", got, want)
	}
}
