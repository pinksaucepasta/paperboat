//go:build windows

package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func lockedFailure(path string) error {
	return &fs.PathError{Op: "unlinkat", Path: path, Err: windows.ERROR_SHARING_VIOLATION}
}

func TestLockedBridgeLeftoverIsOnlyAnEmptyOwnDirectoryNamedByTheHandle(t *testing.T) {
	base := filepath.Join(t.TempDir(), "elevation")
	empty := filepath.Join(base, "request-0123abcd")
	full := filepath.Join(base, "request-4567ef01")
	for _, directory := range []string{empty, full} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(full, "request.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, ok := lockedBridgeLeftover(lockedFailure(empty)); !ok || path != empty {
		t.Fatalf("an empty locked request directory failed the removal: %q %v", path, ok)
	}
	if _, ok := lockedBridgeLeftover(lockedFailure(full)); ok {
		t.Fatal("a locked request directory with content was tolerated")
	}
	if _, ok := lockedBridgeLeftover(lockedFailure(filepath.Join(base, "other"))); ok {
		t.Fatal("a locked directory that is not a request directory was tolerated")
	}
	if _, ok := lockedBridgeLeftover(lockedFailure(filepath.Join(base, "request-missing"))); ok {
		t.Fatal("a directory that does not exist was tolerated")
	}
	denied := &fs.PathError{Op: "unlinkat", Path: empty, Err: windows.ERROR_ACCESS_DENIED}
	if _, ok := lockedBridgeLeftover(denied); ok {
		t.Fatal("a refusal other than a sharing violation was tolerated")
	}
}

// The review's symlink/junction finding: a request-* entry that is a link to an empty directory elsewhere must not
// be reported as a leftover (the old check followed it with ReadDir).
func TestLockedBridgeLeftoverRefusesAJunction(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(root, "elsewhere")
	base := filepath.Join(root, "elevation")
	link := filepath.Join(base, "request-evil")
	for _, directory := range []string{elsewhere, base} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := exec.Command("cmd", "/c", "mklink", "/J", link, elsewhere).CombinedOutput(); err != nil {
		t.Skipf("no junction on this machine: %v: %s", err, output)
	}
	if _, ok := lockedBridgeLeftover(lockedFailure(link)); ok {
		t.Fatal("a junction to an empty directory was reported as a leftover")
	}
}

func TestUninstallStatusLeftoverIsAcceptedOnlyOnCompletedAndAsCleanPaths(t *testing.T) {
	good := windowsUninstallStatus{State: "completed", Leftover: []string{`C:\Users\u\AppData\Local\paperboat\elevation\request-1`}}
	if !validWindowsUninstallLeftover(good) {
		t.Fatal("a completed status with a clean leftover was refused")
	}
	for name, bad := range map[string]windowsUninstallStatus{
		"failed":   {State: "failed", Leftover: good.Leftover},
		"relative": {State: "completed", Leftover: []string{`elevation\request-1`}},
		"unclean":  {State: "completed", Leftover: []string{`C:\Users\u\..\x`}},
		"many":     {State: "completed", Leftover: []string{`C:\a`, `C:\b`, `C:\c`, `C:\d`, `C:\e`, `C:\f`, `C:\g`, `C:\h`, `C:\i`}},
	} {
		if validWindowsUninstallLeftover(bad) {
			t.Errorf("%s leftover list was accepted", name)
		}
	}
}

// A helper that is alive but slow to acknowledge is a pending uninstall, not a failed one.
func TestSlowButRunningUninstallHelperIsPendingNotFailure(t *testing.T) {
	previous := windowsHelperIsRunning
	t.Cleanup(func() { windowsHelperIsRunning = previous })
	timedOut := windowsUninstallLocalFailure("", context.DeadlineExceeded)
	windowsHelperIsRunning = func(string) (bool, error) { return true, nil }
	if !pendingWindowsUninstallHandoff(timedOut, "helper.exe") {
		t.Fatal("a running helper that was slow to acknowledge failed the uninstall")
	}
	if pendingWindowsUninstallHandoff(errors.New("invalid protected Windows uninstall status"), "helper.exe") {
		t.Fatal("another handoff failure was treated as pending")
	}
	windowsHelperIsRunning = func(string) (bool, error) { return false, nil }
	if pendingWindowsUninstallHandoff(timedOut, "helper.exe") {
		t.Fatal("a helper that is gone was treated as pending")
	}
	windowsHelperIsRunning = func(string) (bool, error) { return true, errors.New("query failed") }
	if pendingWindowsUninstallHandoff(timedOut, "helper.exe") {
		t.Fatal("a failed process query was treated as pending")
	}
}

func TestAppendLeftoverReportsEachPathOnce(t *testing.T) {
	var leftovers []string
	for i := 0; i < 5; i++ { // every retry of the same removal names the same directory
		leftovers = appendLeftover(leftovers, `C:\Users\u\AppData\Local\Paperboat\elevation\request-0123abcd`)
	}
	leftovers = appendLeftover(leftovers, `c:\users\u\appdata\local\paperboat\elevation\REQUEST-0123ABCD`)
	leftovers = appendLeftover(leftovers, `C:\Users\u\AppData\Local\Paperboat\elevation\request-4567ef01`)
	if len(leftovers) != 2 {
		t.Fatalf("leftovers = %v, want the two distinct directories once each", leftovers)
	}
}

func TestAHelperRunningDespiteALaunchErrorIsAPendingUninstallNotAFailure(t *testing.T) {
	original := windowsHelperIsRunning
	t.Cleanup(func() { windowsHelperIsRunning = original })
	helper := `C:\Users\u\AppData\Local\Temp\Paperboat Uninstall\x\paperboat-uninstall-helper.exe`
	windowsHelperIsRunning = func(path string) (bool, error) { return path == helper, nil }
	if !helperStartedDespite(helper) {
		t.Fatal("a running helper was treated as a failed launch")
	}
	if helperStartedDespite(`C:\other\paperboat-uninstall-helper.exe`) {
		t.Fatal("another image counted as this helper")
	}
	windowsHelperIsRunning = func(string) (bool, error) { return false, nil }
	if helperStartedDespite(helper) {
		t.Fatal("a helper that is not running counted as started")
	}
	windowsHelperIsRunning = func(string) (bool, error) { return true, errors.New("process query failed") }
	if helperStartedDespite(helper) {
		t.Fatal("an unreadable process list counted as started")
	}
}

func TestHandoffWaitReadsThroughATransientStatusReadError(t *testing.T) {
	originalRead, originalTimeout := readUninstallStatus, windowsHandoffTimeout
	t.Cleanup(func() { readUninstallStatus, windowsHandoffTimeout = originalRead, originalTimeout })
	windowsHandoffTimeout = 5 * time.Second
	statusPath := `C:\Users\u\AppData\Local\Temp\Paperboat Uninstall\x\status.json`
	reads := 0
	readUninstallStatus = func(string) (windowsUninstallStatus, error) {
		reads++
		if reads <= 2 { // the helper replaces status.json while it is read
			return windowsUninstallStatus{}, &fs.PathError{Op: "open", Path: statusPath, Err: windows.ERROR_SHARING_VIOLATION}
		}
		return windowsUninstallStatus{State: "completed"}, nil
	}
	if err := waitForWindowsUninstallHandoff(context.Background(), statusPath); err != nil || reads != 3 {
		t.Fatalf("a transient sharing violation failed the handoff: err=%v reads=%d", err, reads)
	}
}

func TestHandoffWaitFailsWithTheReadErrorWhenTheStatusStaysUnreadable(t *testing.T) {
	originalRead, originalTimeout := readUninstallStatus, windowsHandoffTimeout
	t.Cleanup(func() { readUninstallStatus, windowsHandoffTimeout = originalRead, originalTimeout })
	windowsHandoffTimeout = 300 * time.Millisecond
	statusPath := `C:\Users\u\AppData\Local\Temp\Paperboat Uninstall\x\status.json`
	broken := errors.New("invalid protected Windows uninstall status")
	readUninstallStatus = func(string) (windowsUninstallStatus, error) { return windowsUninstallStatus{}, broken }
	err := waitForWindowsUninstallHandoff(context.Background(), statusPath)
	if err == nil || !errors.Is(err, broken) {
		t.Fatalf("a status that never validates must fail with its own error, got %v", err)
	}
}
