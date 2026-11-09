//go:build windows

package hostinstall

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestMain lets a copy of this test binary stand in for a running program whose image must be freed.
func TestMain(m *testing.M) {
	if os.Getenv("PB_TEST_STAY_RUNNING") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin) // until the test closes stdin
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// victus, pb update --from: the rollback of a failed update had to free the rollback slot, which was the image of
// the user's still-running `pb update`. os.Remove answered "Access is denied" and the rollback stopped with every
// service down.
func TestRemoveWindowsFileFreesTheSlotOfARunningImage(t *testing.T) {
	registered := 0
	restore := deleteWindowsFileAtRestart
	deleteWindowsFileAtRestart = func(*uint16) error { registered++; return nil }
	t.Cleanup(func() { deleteWindowsFileAtRestart = restore })

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(t.TempDir(), "pb.rollback.exe")
	if err := os.WriteFile(slot, body, 0o700); err != nil {
		t.Fatal(err)
	}
	running := exec.Command(slot)
	running.Env = append(os.Environ(), "PB_TEST_STAY_RUNNING=1")
	stdin, err := running.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = stdin.Close()
			_ = running.Wait()
		}
	}
	t.Cleanup(stop)
	if err := os.Remove(slot); err == nil {
		t.Fatal("fixture: Windows deleted the image of a running program")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := removeWindowsFileWithRetry(ctx, slot); err != nil {
		t.Fatalf("free the slot of a running image: %v", err)
	}
	if _, err := os.Lstat(slot); !os.IsNotExist(err) {
		t.Fatalf("the slot is still occupied: %v", err)
	}
	retired, _ := filepath.Glob(slot + windowsRetiredSuffix + "*")
	if len(retired) != 1 || registered != 1 {
		t.Fatalf("retired files=%v, restart deletions registered=%d; want one of each", retired, registered)
	}

	// Once the program has exited, the next use of the slot removes what was moved aside.
	stop()
	if err := removeWindowsFileWithRetry(ctx, slot); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(slot + windowsRetiredSuffix + "*"); len(left) != 0 {
		t.Fatalf("retired image left behind after its process exited: %v", left)
	}
}
