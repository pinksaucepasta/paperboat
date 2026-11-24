//go:build linux || darwin

package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/store"
)

// PB_BENCH_ROOT names a directory on the disk under test.
func BenchmarkRecoverAfterRestart(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				root, err := os.MkdirTemp(os.Getenv("PB_BENCH_ROOT"), "recover-")
				root = filepath.Join(root, "state")
				if err != nil {
					b.Fatal(err)
				}
				state := runningRecords(b, root, count)
				b.StartTimer()
				manager, err := NewManager(ManagerConfig{Store: state, Launch: func(pty.Command) (PTYProcess, error) { return nil, ErrInvalidSession }})
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				_ = manager.Shutdown(context.Background())
				_ = state.Close()
				_ = os.RemoveAll(filepath.Dir(root))
				b.StartTimer()
			}
		})
	}
}

func runningRecords(t testing.TB, root string, count int) *store.Store {
	t.Helper()
	ctx := context.Background()
	state, err := store.Open(ctx, store.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for i := range count {
		id := fmt.Sprintf("ses_%d", i)
		if err := state.CreateSession(ctx, store.Session{ID: id, Name: id, CWD: root, CommandPath: "/bin/sh", Columns: 80, Rows: 24, State: string(Running), Generation: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := state.AppendOutput(ctx, id, 1, 0, []byte("output"), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// A crash may repeat recovery.
func TestRecoveryRepeats(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	state := runningRecords(t, root, 3)
	defer state.Close()
	launch := func(pty.Command) (PTYProcess, error) { return nil, ErrInvalidSession }
	for range 2 {
		manager, err := NewManager(ManagerConfig{Store: state, Launch: launch})
		if err != nil {
			t.Fatal(err)
		}
		for _, snapshot := range manager.List() {
			if snapshot.State != Exited {
				t.Fatalf("recovered as %s", snapshot.State)
			}
		}
	}
}
