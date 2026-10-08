//go:build darwin

package machineguard

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDarwinDefaultControlPathAvoidsSystemRunDirectory(t *testing.T) {
	parent := filepath.Dir(DefaultStateDir)
	want := filepath.Join(parent, "machineguard-control", "control.sock")
	if DefaultSocket != want || filepath.Dir(DefaultSocket) == DefaultStateDir {
		t.Fatalf("control socket %q must use a separate directory under %q", DefaultSocket, parent)
	}
}

func TestDarwinDefaultGuardAcceptsNonrootClient(t *testing.T) {
	if os.Getenv("PAPERBOAT_DARWIN_DEFAULT_GUARD_TEST") != "1" {
		t.Skip("requires the installed macOS guard and an authorized non-root client")
	}
	if os.Geteuid() == 0 {
		t.Fatal("default guard access must be checked as a non-root client")
	}
	info, err := os.Stat(filepath.Dir(DefaultSocket))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("control directory must permit traversal under launchd umask: %v, %v", info, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := Connect(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Status(ctx); err != nil {
		t.Fatal(err)
	}
}
