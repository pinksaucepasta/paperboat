//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"os"
	"testing"
	"time"
)

// Run only as the isolated enrolled account on an approved native target. This
// exercises the same canonical service cutover used by completed bootstrap.
func TestNativeBootstrapDaemonCanonicalCutover(t *testing.T) {
	version := os.Getenv("PAPERBOAT_TEST_BOOTSTRAP_DAEMON_VERSION")
	if version == "" {
		t.Skip("set PAPERBOAT_TEST_BOOTSTRAP_DAEMON_VERSION on the isolated native installation")
	}
	server := os.Getenv("PAPERBOAT_TEST_BOOTSTRAP_SERVER")
	if server == "" {
		t.Fatal("PAPERBOAT_TEST_BOOTSTRAP_SERVER is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := bindBootstrapDaemon(ctx, server, version); err != nil {
		t.Fatal(err)
	}
}
