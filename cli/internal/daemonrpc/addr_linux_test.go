package daemonrpc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootDaemonSocketIgnoresTheLoginSession(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/0")
	if got, want := unixSocketAddress(0), filepath.Join(os.TempDir(), "paperboat-0", "daemon.sock"); got != want {
		t.Fatalf("root socket = %q, want %q", got, want)
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := unixSocketAddress(1000); got != "/run/user/1000/paperboat/daemon.sock" {
		t.Fatalf("user socket = %q", got)
	}
}
