package hostruntimecmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// On macOS a failing hostd waits a growing, capped, jittered backoff persisted across launchd's restarts; a
// stable run starts a new streak, a clean exit clears it, and launchd stopping the job ends the wait at once.
func TestHostdPacesLaunchdRestartsWithAPersistedBackoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostd", "startup-backoff.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	sleep := func(_ context.Context, wait time.Duration) { slept = append(slept, wait) }
	for failures, first := 1, hostdStartupRetryFirst; failures <= 3; failures++ {
		want := first << (failures - 1)
		wait := paceHostdExit(context.Background(), path, time.Second, true, sleep)
		if wait < want/2 || wait >= want/2*3 || slept[len(slept)-1] != wait {
			t.Fatalf("failure %d waited %v, want %v jittered over [%v, %v)", failures, wait, want, want/2, want/2*3)
		}
	}
	if state := readHostdStartupPacing(path); state.Failures != 3 {
		t.Fatalf("persisted failures = %d, want 3 across launches", state.Failures)
	}
	if wait := paceHostdExit(context.Background(), path, 2*hostdStableRun, true, sleep); wait >= hostdStartupRetryFirst/2*3 {
		t.Fatalf("a failure after a stable run waited %v, want a new streak", wait)
	}
	for range 40 {
		paceHostdExit(context.Background(), path, time.Second, true, sleep)
	}
	if wait := slept[len(slept)-1]; wait < hostdStartupRetryMaximum/2 || wait >= hostdStartupRetryMaximum/2*3 {
		t.Fatalf("a long streak waited %v, want the capped %v jittered", wait, hostdStartupRetryMaximum)
	}
	if wait := paceHostdExit(context.Background(), path, time.Second, false, sleep); wait != 0 {
		t.Fatalf("a clean exit waited %v", wait)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a clean exit kept the failure streak: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	paceHostdExit(ctx, path, time.Second, true, sleepContext)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stopping hostd waited %v for the backoff", elapsed)
	}
}
