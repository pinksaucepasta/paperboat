//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

// stubStagedRuntime writes plainly and fails the named step (once its call count reaches failAt); the real write
// needs the restore privilege and the real checks a signed, SYSTEM-owned file.
func stubStagedRuntime(t *testing.T, fail string, failAt int) {
	t.Helper()
	write, check := writeStagedWindowsRuntime, stagedWindowsRuntimeCheck
	t.Cleanup(func() { writeStagedWindowsRuntime, stagedWindowsRuntimeCheck = write, check })
	calls := map[string]int{}
	writeStagedWindowsRuntime = func(path string, body []byte, _ string) error {
		calls["write"]++
		if fail == "write" && calls["write"] >= failAt {
			return errors.New("injected write failure")
		}
		return os.WriteFile(path, body, 0o600)
	}
	stagedWindowsRuntimeCheck = func(name string, _ func() error) error {
		calls[name]++
		if name == fail && calls[name] >= failAt {
			return errors.New("injected " + name + " failure")
		}
		return nil
	}
}

func stagedFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".paperboat-runtime-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func stagedSource() installsource.Source {
	return installsource.Source{Version: "1", Platform: "windows", Architecture: runtime.GOARCH, SHA256: strings.Repeat("a", 64), Length: 3, Distribution: installsource.Official}
}

// Review of 5356e280a: every failure after the staged file was written returned a nil discard, the deferred cleanup
// called it and panicked, and the staged runtime stayed behind.
func TestWindowsStagedRuntimeFailureLeavesNothing(t *testing.T) {
	for _, step := range []string{"write", "format", "bytes", "authenticode", "owner"} {
		t.Run(step, func(t *testing.T) {
			stubStagedRuntime(t, step, 1)
			dir := t.TempDir()
			publish, discard, err := stageWindowsRuntimeFile(context.Background(), []byte("new"), filepath.Join(dir, "pb.exe"), filepath.Join(dir, "pb.rollback.exe"), stagedSource(), "")
			if err == nil || publish != nil || discard != nil {
				t.Fatalf("failure at %s: err %v, publish %v, discard %v", step, err, publish != nil, discard != nil)
			}
			if left := stagedFiles(t, dir); len(left) != 0 {
				t.Fatalf("failure at %s left %v", step, left)
			}
		})
	}
}

// The staged file waits across the cutover; publish verifies it again and refuses bytes swapped in that time
// before it renames anything (review of 5356e280a).
func TestWindowsStagedRuntimeSwappedBeforePublishIsRefused(t *testing.T) {
	for _, step := range []string{"format", "bytes", "authenticode", "owner"} {
		t.Run(step, func(t *testing.T) {
			stubStagedRuntime(t, step, 2) // passes at staging, fails at publish
			dir := t.TempDir()
			current, rollback := filepath.Join(dir, "pb.exe"), filepath.Join(dir, "pb.rollback.exe")
			if err := os.WriteFile(current, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			publish, discard, err := stageWindowsRuntimeFile(context.Background(), []byte("new"), current, rollback, stagedSource(), "")
			if err != nil {
				t.Fatal(err)
			}
			if err := publish(context.Background()); err == nil {
				t.Fatalf("a staged runtime failing %s at publish was published", step)
			}
			if body, err := os.ReadFile(current); err != nil || string(body) != "old" {
				t.Fatalf("the slot changed after a refused publish: %q %v", body, err)
			}
			if _, err := os.Stat(rollback); !os.IsNotExist(err) {
				t.Fatalf("a refused publish rotated the slot: %v", err)
			}
			if err := discard(); err != nil || len(stagedFiles(t, dir)) != 0 {
				t.Fatalf("discard left %v: %v", stagedFiles(t, dir), err)
			}
		})
	}
	stubStagedRuntime(t, "", 0)
	dir := t.TempDir()
	current, rollback := filepath.Join(dir, "pb.exe"), filepath.Join(dir, "pb.rollback.exe")
	if err := os.WriteFile(current, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	publish, discard, err := stageWindowsRuntimeFile(context.Background(), []byte("new"), current, rollback, stagedSource(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	newBody, _ := os.ReadFile(current)
	oldBody, _ := os.ReadFile(rollback)
	if string(newBody) != "new" || string(oldBody) != "old" || discard() != nil || len(stagedFiles(t, dir)) != 0 {
		t.Fatalf("publish: current %q rollback %q staged %v", newBody, oldBody, stagedFiles(t, dir))
	}
}
