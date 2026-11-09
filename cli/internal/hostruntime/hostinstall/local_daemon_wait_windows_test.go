//go:build windows

package hostinstall

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/causetext"
)

// A wait that ends without any daemon process says so, instead of a bare deadline.
func TestWaitWindowsLocalDaemonReadyNamesAMissingDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := waitWindowsLocalDaemonReady(ctx, "S-1-5-21-111-222-333-1001", filepath.Join(t.TempDir(), "runtime"))
	if err == nil || !strings.Contains(causetext.Text(err), "the local daemon is not running: no live process is recorded in") {
		t.Fatalf("wait error does not name the missing daemon: %v (%s)", err, causetext.Text(err))
	}
}
