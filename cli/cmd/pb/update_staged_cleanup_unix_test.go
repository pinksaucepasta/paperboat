//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
	"github.com/spf13/cobra"
)

// L46: a staged update kept for a step still finishing (or left by a pb a closed terminal killed) stayed in the
// temporary directory for good (~100 MB, device check 11-27 linuxpool c2/c3, macpool m3). The helper pb starts
// with the staging removes it once pb is gone and no privileged step runs, and never while one does.
func TestStagedUpdateIsRemovedOnceNoStepRuns(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	input := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(input, []byte("candidate bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalInstall, originalStart, originalRunning := installStaged, startStagedCleanup, privilegedStepRunning
	t.Cleanup(func() {
		installStaged, startStagedCleanup, privilegedStepRunning = originalInstall, originalStart, originalRunning
	})
	var started []string
	startStagedCleanup = func(directory string) { started = append(started, directory) }
	var staged string
	installStaged = func(_ *cobra.Command, _ context.Context, path string, _ bool) error {
		staged = path
		return hostruntimecmd.StillFinishingFailure("still restoring the previous version")
	}
	command := &cobra.Command{}
	command.Flags().String("approve", "", "")
	command.SetContext(context.Background())
	_ = actionUpdateFrom(command, input, false)
	directory := filepath.Dir(staged)
	if len(started) != 1 || started[0] != directory {
		t.Fatalf("cleanup helper started for %v, want [%s]", started, directory)
	}
	// pb (this test's pid stand-in) has exited; a step still runs: kept through the bound.
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	privilegedStepRunning = func() bool { return true }
	if waitThenRemoveStaged(directory, exited.Process.Pid, time.Millisecond, 20*time.Millisecond) {
		t.Fatal("removed while a privileged step may still read it")
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged update gone while a step runs: %v", err)
	}
	// While pb itself still runs, it is kept too.
	privilegedStepRunning = func() bool { return false }
	if waitThenRemoveStaged(directory, os.Getpid(), time.Millisecond, 20*time.Millisecond) {
		t.Fatal("removed while pb still runs")
	}
	if !waitThenRemoveStaged(directory, exited.Process.Pid, time.Millisecond, time.Second) {
		t.Fatal("not removed once pb exited and no step runs")
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged update remains: %v", err)
	}
}

// Only pb's own staging directory is removed: never another name, a link, or a path elsewhere.
func TestStagedCleanupRemovesOnlyItsOwnDirectory(t *testing.T) {
	temporary, outside := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", temporary)
	victim := filepath.Join(outside, "keep")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(temporary, "paperboat-update-1")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(temporary, "paperboat-update-proof-1")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(outside, "paperboat-update-2")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, other, nested, "paperboat-update-3"} {
		if removeOwnStagedUpdate(path) {
			t.Fatalf("%s removed", path)
		}
	}
	for _, path := range []string{victim, link, other, nested} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s touched: %v", path, err)
		}
	}
	own := filepath.Join(temporary, "paperboat-update-4")
	if err := os.Mkdir(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(own, "input-1")); err != nil {
		t.Fatal(err)
	}
	if !removeOwnStagedUpdate(own) {
		t.Fatal("own staging directory not removed")
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatalf("a link inside was followed: %v", err)
	}
}
