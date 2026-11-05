//go:build darwin || linux

package localdaemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// pb status and pb doctor list the live generation first, then draining ones with their streams and
// deadlines; a generation whose process is gone or whose drain passed its deadline is dropped.
func TestGenerationStatesListLiveThenDraining(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	live, draining, expired, gone := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)
	for _, state := range []GenerationState{
		{Generation: draining, PID: os.Getpid(), State: "draining", Streams: 2, DrainUntil: now.Add(time.Hour)},
		{Generation: live, PID: os.Getpid(), State: "live", Streams: 1},
		{Generation: expired, PID: os.Getpid(), State: "draining", Streams: 1, DrainUntil: now.Add(-time.Second)},
		{Generation: gone, PID: 1 << 22, State: "draining", Streams: 1, DrainUntil: now.Add(time.Hour)},
	} {
		if err := WriteGenerationState(root, state); err != nil {
			t.Fatal(err)
		}
	}
	states, err := GenerationStates(root, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].Generation != live || states[1].Generation != draining || states[1].Streams != 2 {
		t.Fatalf("states = %+v", states)
	}
	for _, generation := range []string{expired, gone} {
		if _, err := os.Stat(generationStatePath(root, generation)); !os.IsNotExist(err) {
			t.Fatalf("stale state %s kept: %v", generation[:1], err)
		}
	}
	if err := RemoveGenerationState(root, draining); err != nil {
		t.Fatal(err)
	}
	if states, _ = GenerationStates(root, now); len(states) != 1 {
		t.Fatalf("removed state still listed: %+v", states)
	}
}

// The new generation waits for the old one to release the daemon lock instead of failing.
func TestWaitDaemonLockTakesTheLockOnceReleased(t *testing.T) {
	path := filepath.Join(shorttemp.Dir(t), "daemon.lock")
	old, err := acquireProcessLock(path, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = old.Close()
	}()
	lock, err := WaitDaemonLock(context.Background(), path, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
}
