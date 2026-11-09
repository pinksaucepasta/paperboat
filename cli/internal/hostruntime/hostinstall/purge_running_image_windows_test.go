//go:build windows

package hostinstall

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

// startOldWindowsCLI runs a copy of this test binary as bin\pb.exe under root and then moves the running image
// to the rollback slot, as a second install over a running `pb login` does. The process stays until it is ended.
func startOldWindowsCLI(t *testing.T, root string) (image string, exited <-chan struct{}) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(root, "bin", "pb.exe")
	image = filepath.Join(root, "releases", "pb.rollback.exe")
	for _, directory := range []string{filepath.Dir(current), filepath.Dir(image)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(current, body, 0o700); err != nil {
		t.Fatal(err)
	}
	running := exec.Command(current)
	running.Env = append(os.Environ(), "PB_TEST_STAY_RUNNING=1")
	stdin, err := running.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = running.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = running.Process.Kill()
		<-done
	})
	if err := os.Rename(current, image); err != nil {
		t.Fatalf("fixture: move the running image to the rollback slot: %v", err)
	}
	if err := os.Remove(image); err == nil {
		t.Fatal("fixture: Windows deleted the image of a running program")
	}
	return image, done
}

func waitExited(t *testing.T, exited <-chan struct{}) {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the old CLI process is still running")
	}
}

// Windows Sandbox, 2026-11-10: an old `pb login` kept running from releases\pb.rollback.exe; the uninstall removed
// the install root before ending it, got "Access is denied" and reported failure with most of the product left.
func TestAccountSweepEndsARunningImageBeforeRemovingItsRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users", "account")
	_, exited := startOldWindowsCLI(t, root)
	if err := os.RemoveAll(root); err == nil {
		t.Fatal("fixture: the root of a running image was removed")
	}

	if err := terminatePayloadProcesses([]string{root}, ""); err != nil {
		t.Fatalf("end the processes that run from the root: %v", err)
	}
	waitExited(t, exited)
	atRestart, err := removeWindowsAccountPayloadKeepingCLI(context.Background(), root, "")
	if err != nil || atRestart {
		t.Fatalf("remove the root after its processes ended: atRestart=%v err=%v", atRestart, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("the root is still there: %v", err)
	}
}

// A file nobody can free before its holder exits is moved aside and left to Windows; nothing is reported removed
// that is not, and a later run removes it normally.
func TestAccountPayloadStillInUseIsDeletedAtRestart(t *testing.T) {
	var registered []string
	restore := deleteWindowsFileAtRestart
	deleteWindowsFileAtRestart = func(path *uint16) error {
		registered = append(registered, windows.UTF16PtrToString(path))
		return nil
	}
	t.Cleanup(func() { deleteWindowsFileAtRestart = restore })

	root := filepath.Join(t.TempDir(), "users", "account")
	image, exited := startOldWindowsCLI(t, root)
	if err := os.WriteFile(filepath.Join(root, "releases", "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	atRestart, err := removeWindowsAccountPayloadKeepingCLI(context.Background(), root, "")
	if err != nil || !atRestart {
		t.Fatalf("a root with a running image: atRestart=%v err=%v; want it left to the restart", atRestart, err)
	}
	if _, err := os.Lstat(image); !os.IsNotExist(err) {
		t.Fatalf("the image kept its reusable name: %v", err)
	}
	retired, _ := filepath.Glob(image + windowsRetiredSuffix + "*")
	if len(retired) != 1 {
		t.Fatalf("retired images = %v, want one", retired)
	}
	// The file first, then the directories from the inside out: Windows deletes a directory only when empty.
	want := []string{retired[0], filepath.Join(root, "releases"), root}
	if fmt.Sprint(registered) != fmt.Sprint(want) {
		t.Fatalf("registered for deletion at restart:\n got %v\nwant %v", registered, want)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "releases")); len(entries) != 1 {
		t.Fatalf("files that were not in use stayed: %v", entries)
	}
	if _, err := os.Lstat(filepath.Join(root, "bin")); !os.IsNotExist(err) {
		t.Fatalf("an empty directory stayed: %v", err)
	}

	if err := terminatePayloadProcesses([]string{root}, ""); err != nil {
		t.Fatal(err)
	}
	waitExited(t, exited)
	if atRestart, err := removeWindowsAccountPayloadKeepingCLI(context.Background(), root, ""); err != nil || atRestart {
		t.Fatalf("after the process ended: atRestart=%v err=%v", atRestart, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("the root is still there: %v", err)
	}
}

// Security review, 2026-11-10: the sweep read a process ID's user and image, then opened the ID again to end it.
// Windows reuses the IDs of exited processes, so the second open could name, and end, somebody else's process.
func TestPayloadProcessThatIsNotTheListedOneIsNotEnded(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "users", "account")
	_, exited := startOldWindowsCLI(t, root)
	processIDs, err := payloadProcessIDs([]string{root}, "")
	if err != nil || len(processIDs) != 1 {
		t.Fatalf("fixture: processes running from the root = %v, %v; want one", processIDs, err)
	}
	users := payloadProcessUsers("")
	stillRunning := func(reason string) {
		t.Helper()
		select {
		case <-exited:
			t.Fatalf("a process that %s was ended", reason)
		case <-time.After(500 * time.Millisecond):
		}
	}
	// To the sweep a reused ID is an ID whose process, once opened, is not the account's.
	if err := endPayloadProcesses(processIDs, []string{filepath.Join(base, "users", "listed")}, users); err != nil {
		t.Fatal(err)
	}
	stillRunning("runs another image")
	if err := endPayloadProcesses(processIDs, []string{root}, []string{"S-1-5-21-1-2-3-1002"}); err != nil {
		t.Fatal(err)
	}
	stillRunning("runs as another user")
	if err := endPayloadProcesses(processIDs, []string{root}, users); err != nil {
		t.Fatal(err)
	}
	waitExited(t, exited)
}

// The whole purge on a machine without Paperboat: it must end the old CLI, remove the account and succeed.
func TestWindowsPurgeConvergesWhileAnOldCLIIsRunning(t *testing.T) {
	if allowed, err := checkAdministrator(); err != nil || !allowed {
		t.Skip("requires an elevated administrator")
	}
	shared, err := service.DefaultLayout("windows")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{shared.InstallRoot, WindowsProgramDataRoot()} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Skip("requires a machine without a Paperboat installation: the purge is machine-wide")
		}
	}
	if err := secureSharedProgramRoot(shared.InstallRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shared.InstallRoot) })
	ownerSID := fmt.Sprintf("S-1-5-21-49157-49158-49159-%d", uint32(time.Now().UnixNano()))
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.ToLower(layout.InstallRoot), strings.ToLower(shared.InstallRoot)+`\`) {
		t.Fatalf("fixture: account root %q is outside the shared root", layout.InstallRoot)
	}
	_, exited := startOldWindowsCLI(t, layout.InstallRoot)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := Purge(ctx, ownerSID); err != nil {
		t.Fatalf("purge with an old CLI still running: %v (%s)", err, FailureDetail(err))
	}
	waitExited(t, exited)
	for _, path := range []string{layout.InstallRoot, shared.InstallRoot} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", path, err)
		}
	}
}
