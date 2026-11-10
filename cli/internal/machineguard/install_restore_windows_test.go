//go:build windows

package machineguard

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

// TestMain lets a copy of this test binary stand in for a running guard whose image must be replaced.
func TestMain(m *testing.M) {
	if os.Getenv("PB_TEST_STAY_RUNNING") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin) // until the test closes stdin
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startCandidateGuard models an installed update: the prior guard's bytes are snapshotted, a copy of this test
// binary replaces them as the candidate guard and runs. The token runs as an administrator's elevated console
// does, with the backup and restore privileges disabled. stop ends the candidate's process.
func startCandidateGuard(t *testing.T) (rollback *guardFileRollback, installed string, prior []byte, stop func()) {
	t.Helper()
	if adjustRestorePrivilege(t, false) {
		t.Cleanup(func() { adjustRestorePrivilege(t, true) })
	}
	restore := deleteGuardFileAtRestart
	deleteGuardFileAtRestart = func(*uint16) error { return nil }
	t.Cleanup(func() { deleteGuardFileAtRestart = restore })

	dir := t.TempDir()
	installed, prior = filepath.Join(dir, "pb.exe"), []byte("guard 2026.11.9")
	if err := os.WriteFile(installed, prior, 0o700); err != nil {
		t.Fatal(err)
	}
	rollback, err := snapshotGuardFilesValidated(func(string, string, os.FileInfo) error { return nil }, installed)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, ".pb-candidate.exe")
	if err := os.WriteFile(candidate, body, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := replaceStateFile(candidate, installed); err != nil {
		t.Fatal(err)
	}
	rollback.record(installed)
	running := exec.Command(installed)
	running.Env = append(os.Environ(), "PB_TEST_STAY_RUNNING=1")
	stdin, err := running.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			_ = stdin.Close()
			_ = running.Wait()
		}
	}
	t.Cleanup(stop)
	probe := filepath.Join(dir, ".probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceStateFile(probe, installed); err == nil {
		t.Fatal("fixture: Windows replaced the image of a running program")
	}
	return rollback, installed, prior, stop
}

// Windows Sandbox, 2026-11-10: the rollback of a failed update ended the new guard's task and replaced its
// executable at once. `schtasks /End` returns before the process has exited, so the replacement answered "Access is
// denied" and the machine kept the new guard beside the previous pb. The restore waits for the exit.
func TestGuardRestoreWaitsForTheEndedGuardToExit(t *testing.T) {
	rollback, installed, prior, stop := startCandidateGuard(t)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		time.Sleep(time.Second)
		stop()
	}()
	err := rollback.restore()
	<-exited
	if err != nil {
		t.Fatalf("restore the prior guard while the ended guard is still exiting: %v", err)
	}
	if got, err := os.ReadFile(installed); err != nil || !bytes.Equal(got, prior) {
		t.Fatalf("installed guard after restore = %q (%v), want the prior guard", got, err)
	}
	if retired, _ := filepath.Glob(installed + guardRetiredSuffix + "*"); len(retired) != 0 {
		t.Fatalf("a guard that exited in time was moved aside: %v", retired)
	}
}

// A guard process that outlives the wait still gives its place to the prior guard: its image is moved aside and
// removed by the next replacement once the process is gone.
func TestGuardRestoreMovesAStillRunningGuardAside(t *testing.T) {
	rollback, installed, prior, stop := startCandidateGuard(t)
	if err := rollback.restore(); err != nil {
		t.Fatalf("restore the prior guard under a running guard: %v", err)
	}
	if got, err := os.ReadFile(installed); err != nil || !bytes.Equal(got, prior) {
		t.Fatalf("installed guard after restore = %q (%v), want the prior guard", got, err)
	}
	if retired, _ := filepath.Glob(installed + guardRetiredSuffix + "*"); len(retired) != 1 {
		t.Fatalf("retired guard images = %v, want the one still running", retired)
	}
	stop()
	next := filepath.Join(filepath.Dir(installed), ".pb-next.exe")
	if err := os.WriteFile(next, []byte("guard 2026.11.10"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := replaceGuardFile(next, installed); err != nil {
		t.Fatal(err)
	}
	if retired, _ := filepath.Glob(installed + guardRetiredSuffix + "*"); len(retired) != 0 {
		t.Fatalf("the exited guard's image was not removed: %v", retired)
	}
}

// The Undo of an installed guard registers the prior task long after the installation returned, from what
// `schtasks /Query /XML` printed then. The prior definition was kept as a file that the installation removed on
// return, so the restore had nothing to register ("exit status 1") and the prior guard was never started.
func TestPriorGuardTaskIsRegisteredAgainFromItsExport(t *testing.T) {
	if !elevation.IsCurrentProcessElevated() {
		t.Skip("registering a LocalSystem task requires an elevated administrator")
	}
	name, dir := "PaperboatGuardTaskTest-"+strconv.Itoa(os.Getpid()), t.TempDir()
	register := func(definition string) {
		t.Helper()
		path, err := writeGuardTaskFile(dir, definition)
		if err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("schtasks.exe", "/Create", "/TN", name, "/XML", path, "/F").CombinedOutput(); err != nil {
			t.Fatalf("register the task: %v: %s", err, output)
		}
	}
	export := func() string {
		t.Helper()
		output, err := exec.Command("schtasks.exe", "/Query", "/TN", name, "/XML").Output()
		if err != nil {
			t.Fatal(err)
		}
		return decodeTaskXML(output)
	}
	t.Cleanup(func() { _ = exec.Command("schtasks.exe", "/Delete", "/TN", name, "/F").Run() })
	register(taskXML(filepath.Join(dir, "pb.exe"))) // never run: the executable does not exist
	prior := export()
	register(prior)
	if again := export(); !strings.Contains(again, "daemon machine-guard run") || !strings.Contains(again, "S-1-5-18") {
		t.Fatalf("the task registered from its export lost its definition:\n%s", again)
	}
}
