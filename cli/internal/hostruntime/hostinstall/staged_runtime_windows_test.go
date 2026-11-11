//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"fmt"
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

// Review of 5356e280a: the previous rollback image was deleted before current took its place, so a failure in
// either later rename lost the image a rollback needs. It is moved aside and put back on failure.
func TestWindowsRuntimeSlotRotationKeepsTheRollbackImage(t *testing.T) {
	for _, failing := range []int{1, 2, 3} { // 1: rollback aside, 2: current to rollback, 3: staged to current
		t.Run(fmt.Sprintf("rename%d", failing), func(t *testing.T) {
			rename := renameWindowsRuntimeSlot
			t.Cleanup(func() { renameWindowsRuntimeSlot = rename })
			calls := 0
			renameWindowsRuntimeSlot = func(from, to string) error {
				calls++
				if calls == failing {
					return errors.New("injected rename failure")
				}
				return os.Rename(from, to)
			}
			dir := t.TempDir()
			staged, current, rollback := filepath.Join(dir, "staged.exe"), filepath.Join(dir, "pb.exe"), filepath.Join(dir, "pb.rollback.exe")
			for path, body := range map[string]string{staged: "new", current: "current", rollback: "previous"} {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := rotateWindowsRuntimeSlot(context.Background(), staged, current, rollback); err == nil {
				t.Fatal("a failed rename was reported as success")
			}
			for path, want := range map[string]string{current: "current", rollback: "previous", staged: "new"} {
				if body, err := os.ReadFile(path); err != nil || string(body) != want {
					t.Fatalf("after rename %d failed: %s = %q (%v), want %q", failing, filepath.Base(path), body, err, want)
				}
			}
			if _, err := os.Stat(rollback + ".aside"); !os.IsNotExist(err) {
				t.Fatalf("the aside copy was left: %v", err)
			}
		})
	}
	dir := t.TempDir()
	staged, current, rollback := filepath.Join(dir, "staged.exe"), filepath.Join(dir, "pb.exe"), filepath.Join(dir, "pb.rollback.exe")
	for path, body := range map[string]string{staged: "new", current: "current", rollback: "previous"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := rotateWindowsRuntimeSlot(context.Background(), staged, current, rollback); err != nil {
		t.Fatal(err)
	}
	newBody, _ := os.ReadFile(current)
	oldBody, _ := os.ReadFile(rollback)
	_, asideErr := os.Stat(rollback + ".aside")
	if string(newBody) != "new" || string(oldBody) != "current" || !os.IsNotExist(asideErr) {
		t.Fatalf("rotation: current %q rollback %q aside %v", newBody, oldBody, asideErr)
	}
}

// Review of 930d4805e: with current missing, publishing the staged image must not delete the only good image left.
func TestWindowsRuntimeSlotRotationWithoutCurrentKeepsTheRollback(t *testing.T) {
	dir := t.TempDir()
	staged, current, rollback := filepath.Join(dir, "staged.exe"), filepath.Join(dir, "pb.exe"), filepath.Join(dir, "pb.rollback.exe")
	for path, body := range map[string]string{staged: "new", rollback: "previous"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := rotateWindowsRuntimeSlot(context.Background(), staged, current, rollback); err != nil {
		t.Fatal(err)
	}
	newBody, _ := os.ReadFile(current)
	oldBody, err := os.ReadFile(rollback)
	if string(newBody) != "new" || err != nil || string(oldBody) != "previous" {
		t.Fatalf("current %q, rollback %q (%v): want new and previous", newBody, oldBody, err)
	}
}
