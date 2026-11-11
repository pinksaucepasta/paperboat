//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func plantJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, output)
	}
}

// Security review, 2026-11-10: the elevated uninstall helper removes user-writable paths. A junction planted in
// or above one turned that into an administrator's delete of whatever it points at.
func TestWindowsUserStateRemovalNeverFollowsAPlantedJunction(t *testing.T) {
	profile, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "keep", "sentinel.txt")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("not Paperboat's"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	survived := func(t *testing.T) {
		t.Helper()
		if body, err := os.ReadFile(sentinel); err != nil || string(body) != "not Paperboat's" {
			t.Fatalf("the file behind the junction was touched: %v", err)
		}
	}

	t.Run("inside the state root it is removed as an entry", func(t *testing.T) {
		state := filepath.Join(profile, "AppData", "Local", "Paperboat")
		plantJunction(t, filepath.Join(state, "runtime"), outside)
		if err := os.WriteFile(filepath.Join(state, "credentials.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := retryWindowsRemoval(ctx, profile, state, nil, nil); err != nil {
			t.Fatalf("remove a state root that holds a junction: %v", err)
		}
		if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the state root is still there: %v", err)
		}
		survived(t)
	})
	t.Run("beside a preserved Inbox it is removed as an entry", func(t *testing.T) {
		state := filepath.Join(profile, "Paperboat")
		inbox := filepath.Join(state, "Inbox")
		if err := os.MkdirAll(inbox, 0o700); err != nil {
			t.Fatal(err)
		}
		received := filepath.Join(inbox, "received.txt")
		if err := os.WriteFile(received, []byte("user file"), 0o600); err != nil {
			t.Fatal(err)
		}
		plantJunction(t, filepath.Join(state, "link"), outside)
		if err := retryWindowsRemoval(ctx, profile, state, []string{inbox}, nil); err != nil {
			t.Fatalf("remove a state root that holds the Inbox and a junction: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(state, "link")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the junction is still there: %v", err)
		}
		if _, err := os.Lstat(received); err != nil {
			t.Fatalf("the Inbox was not preserved: %v", err)
		}
		survived(t)
	})
	for name, path := range map[string]string{
		"the state root itself":            filepath.Join(profile, "AppData", "Roaming", "Paperboat"),
		"a directory above the state root": filepath.Join(profile, "Linked", "keep"),
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			link := path
			if filepath.Base(path) == "keep" {
				link = filepath.Dir(path)
			}
			plantJunction(t, link, outside)
			if err := retryWindowsRemoval(ctx, profile, path, nil, nil); !errors.Is(err, errWindowsUserStateRefused) {
				t.Fatalf("a path through a junction: %v, want the refusal", err)
			}
			if _, err := os.Lstat(link); err != nil {
				t.Fatalf("the refused junction was changed: %v", err)
			}
			survived(t)
		})
	}
	t.Run("outside the profile is refused", func(t *testing.T) {
		for _, path := range []string{filepath.Dir(sentinel), profile, filepath.Dir(profile)} {
			if err := retryWindowsRemoval(ctx, profile, path, nil, nil); !errors.Is(err, errWindowsUserStateRefused) {
				t.Fatalf("%s: %v, want the refusal", path, err)
			}
		}
		survived(t)
	})
}

// Security review, 2026-11-10: the reparse check and the removal are two steps, and the user can plant the
// junction between them (the helper retries every 250 ms). No path check closes that, so the removal runs as the
// user without the helper's elevation: behind the junction, what only an administrator may delete survives.
func TestWindowsUserStateRemovalAfterAPlantedJunctionHasOnlyTheUsersRights(t *testing.T) {
	if !elevation.IsCurrentProcessElevated() {
		t.Skip("requires an elevated administrator: only then does the process hold rights its user lacks")
	}
	profile, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Administrators and LocalSystem only, set before the content exists so that it inherits exactly this.
	protected := filepath.Join(outside, "administrators")
	if err := os.Mkdir(protected, 0o700); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(protected, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(protected, "keep", "sentinel.txt")
	if err := os.Mkdir(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("not Paperboat's"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The state path is <profile>\Linked\keep. After the check passed, "Linked" becomes a junction to the
	// protected directory, so the same path now names the administrators' "keep".
	parent := filepath.Join(profile, "Linked")
	state := filepath.Join(parent, "keep")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted := false
	afterWindowsUserStateCheck = func() {
		if planted {
			return
		}
		planted = true
		// On another thread: this one carries the user's token for the removal.
		done := make(chan error, 1)
		go func() {
			if err := os.Rename(parent, parent+".real"); err != nil {
				done <- err
				return
			}
			output, err := exec.Command("cmd", "/c", "mklink", "/J", parent, protected).CombinedOutput()
			if err != nil {
				err = fmt.Errorf("mklink /J: %w: %s", err, output)
			}
			done <- err
		}()
		if err := <-done; err != nil {
			t.Errorf("fixture: plant the junction after the check: %v", err)
		}
	}
	t.Cleanup(func() { afterWindowsUserStateCheck = func() {} })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = retryWindowsRemoval(ctx, profile, state, nil, nil)
	if !planted {
		t.Fatal("fixture: the junction was never planted")
	}
	if body, readErr := os.ReadFile(sentinel); readErr != nil || string(body) != "not Paperboat's" {
		t.Fatalf("the administrators' file behind the junction was deleted with the helper's rights: %v", readErr)
	}
	// The attempt is denied as the user; the helper's own rights then go by handle, which shows the junction.
	if !errors.Is(err, errWindowsUserStateRefused) {
		t.Fatalf("removal through a junction planted after the check: %v, want the refusal", err)
	}
}

// administratorsOnly makes directory one that only Administrators and LocalSystem can use; what is created in it
// afterwards inherits exactly that. It needs an elevated process.
func administratorsOnly(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setTestOwner(t *testing.T, owner *windows.SID, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, owner, nil, nil, nil); err != nil {
			t.Fatalf("fixture: set the owner of %s: %v", path, err)
		}
	}
}

// countElevatedRemovals counts how often a removal needed the helper's elevation.
func countElevatedRemovals(t *testing.T) *int {
	t.Helper()
	count := 0
	removeWindowsStateElevated = func(path string, preserved []string) error {
		count++
		return removeWindowsStateByHandle(path, preserved)
	}
	t.Cleanup(func() { removeWindowsStateElevated = removeWindowsStateByHandle })
	return &count
}

// Security review, 2026-11-10: user state is removed as the user. What the user owns never needs more.
func TestWindowsUserStateOwnedByTheUserIsRemovedWithTheUsersRights(t *testing.T) {
	profile, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(profile, "AppData", "Local", "Paperboat")
	file := filepath.Join(state, "runtime", "state.json")
	writeTestFile(t, file)
	// An elevated run creates them with the Administrators owner; a user's session with the user's.
	setTestOwner(t, user.User.Sid, state, filepath.Dir(file), file)
	elevated := countElevatedRemovals(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := retryWindowsRemoval(ctx, profile, state, nil, nil); err != nil {
		t.Fatalf("remove user-owned state: %v", err)
	}
	if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the state directory is still there: %v", err)
	}
	if *elevated != 0 {
		t.Fatalf("user-owned state was removed with the helper's elevation %d times", *elevated)
	}
}

// Security review, 2026-11-10: since the removal runs as the user, state that only Administrators may delete was
// retried until the plan expired and the uninstall failed. An elevated pb and the account's services leave such
// state in the profile. It is removed with the helper's rights, by handle, and no link in it is followed.
func TestWindowsUserStateOwnedByAdministratorsIsRemoved(t *testing.T) {
	if !elevation.IsCurrentProcessElevated() {
		t.Skip("requires an elevated administrator: only then can Administrators-owned state be created")
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(outside, "administrators")
	administratorsOnly(t, protected)
	sentinel := filepath.Join(protected, "keep", "sentinel.txt")
	writeTestFile(t, sentinel)
	survived := func(t *testing.T, path string) {
		t.Helper()
		if body, err := os.ReadFile(path); err != nil || string(body) != "kept" {
			t.Fatalf("%s was touched: %v", path, err)
		}
	}
	remove := func(t *testing.T, profile, state string, preserved []string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := retryWindowsRemoval(ctx, profile, state, preserved, nil); err != nil {
			t.Fatalf("remove Administrators-owned state: %v", err)
		}
	}
	newProfile := func(t *testing.T) (profile, state string) {
		t.Helper()
		profile, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return profile, filepath.Join(profile, "AppData", "Local", "Paperboat")
	}

	t.Run("what an elevated pb left", func(t *testing.T) {
		profile, state := newProfile(t)
		file := filepath.Join(state, "credentials", "token.json")
		writeTestFile(t, file)
		setTestOwner(t, administrators, state, filepath.Dir(file), file)
		elevated := countElevatedRemovals(t)
		remove(t, profile, state, nil)
		if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the state directory is still there: %v", err)
		}
		// The owner does not decide who may delete: the access list still grants the user here.
		t.Logf("removals that needed the helper's elevation: %d", *elevated)
	})
	t.Run("what only Administrators may delete", func(t *testing.T) {
		profile, state := newProfile(t)
		writeTestFile(t, filepath.Join(state, "config.json"))
		locked := filepath.Join(state, "runtime")
		administratorsOnly(t, locked)
		writeTestFile(t, filepath.Join(locked, "store", "state.json"))
		plantJunction(t, filepath.Join(locked, "link"), protected)
		elevated := countElevatedRemovals(t)
		remove(t, profile, state, nil)
		if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the state directory is still there: %v", err)
		}
		if *elevated == 0 {
			t.Fatal("fixture: the user's rights were enough for an Administrators-only directory")
		}
		survived(t, sentinel)
	})
	t.Run("beside a preserved Inbox", func(t *testing.T) {
		profile, _ := newProfile(t)
		state := filepath.Join(profile, "Paperboat")
		inbox := filepath.Join(state, "Inbox")
		received := filepath.Join(inbox, "received.txt")
		writeTestFile(t, received)
		locked := filepath.Join(state, "runtime")
		administratorsOnly(t, locked)
		writeTestFile(t, filepath.Join(locked, "state.json"))
		plantJunction(t, filepath.Join(locked, "link"), protected)
		remove(t, profile, state, []string{inbox})
		entries, err := os.ReadDir(state)
		if err != nil || len(entries) != 1 || entries[0].Name() != "Inbox" {
			t.Fatalf("left in the state directory: %v, %v; want only the Inbox", entries, err)
		}
		survived(t, received)
		survived(t, sentinel)
	})
	t.Run("what belongs to another account is refused", func(t *testing.T) {
		profile, state := newProfile(t)
		foreign := filepath.Join(state, "runtime")
		administratorsOnly(t, foreign)
		kept := filepath.Join(foreign, "state.json")
		writeTestFile(t, kept)
		localService, err := windows.CreateWellKnownSid(windows.WinLocalServiceSid)
		if err != nil {
			t.Fatal(err)
		}
		if err := windowssecurity.WithRestorePrivilege(func() error {
			return windows.SetNamedSecurityInfo(foreign, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, localService, nil, nil, nil)
		}); err != nil {
			t.Fatalf("fixture: give the directory to another account: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// The owner is read from the open directory right before it would be emptied, not from the plan.
		if err := retryWindowsRemoval(ctx, profile, state, nil, nil); !errors.Is(err, errWindowsUserStateRefused) {
			t.Fatalf("a directory of another account: %v, want the refusal", err)
		}
		survived(t, kept)
	})
	t.Run("through a junction it is refused", func(t *testing.T) {
		profile, _ := newProfile(t)
		// <profile>\Linked is a junction to the Administrators-only directory: the path names its "keep".
		plantJunction(t, filepath.Join(profile, "Linked"), protected)
		if err := removeWindowsStateByHandle(filepath.Join(profile, "Linked", "keep"), nil); !errors.Is(err, errWindowsUserStateRefused) {
			t.Fatalf("a path through a junction: %v, want the refusal", err)
		}
		survived(t, sentinel)
	})
}

// Security review, 2026-11-10: the elevated helper wrote its status file by path in a directory the user can
// write; a link planted there made it an administrator's write to whatever the link named. The helper's files
// are written and read as the user now.
func TestWindowsUninstallHelperFilesAreWrittenWithTheUsersRights(t *testing.T) {
	if !elevation.IsCurrentProcessElevated() {
		t.Skip("requires an elevated administrator: only then does the process hold rights its user lacks")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(base, "administrators")
	administratorsOnly(t, protected)
	sentinel := filepath.Join(protected, "sentinel.txt")
	writeTestFile(t, sentinel)
	if err := os.Mkdir(filepath.Join(protected, "helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	untouched := func(t *testing.T) {
		t.Helper()
		if body, err := os.ReadFile(sentinel); err != nil || string(body) != "kept" {
			t.Fatalf("the administrators' file behind the link was written: %q, %v", body, err)
		}
	}
	status := windowsUninstallStatus{Schema: windowsUninstallStatusSchema, State: "removing", UpdatedAt: time.Now().UTC()}
	helperDirectory := func(t *testing.T) string {
		t.Helper()
		directory := filepath.Join(base, t.Name())
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := protectWindowsUninstallPath(directory, true); err != nil {
			t.Fatal(err)
		}
		return directory
	}

	t.Run("a helper directory swapped for a junction", func(t *testing.T) {
		// The junction is above the directory, so the directory itself looks real to every path check.
		linked := filepath.Join(base, "linked")
		plantJunction(t, linked, protected)
		if err := writeWindowsUninstallStatus(filepath.Join(linked, "helper", "status.json"), status); err == nil {
			t.Fatal("the status was written into a directory only Administrators can write")
		}
		if entries, err := os.ReadDir(filepath.Join(protected, "helper")); err != nil || len(entries) != 0 {
			t.Fatalf("written into the Administrators' directory: %v, %v", entries, err)
		}
	})
	for _, name := range []string{"status.json.new", "status.json"} {
		t.Run("a link planted at "+name, func(t *testing.T) {
			directory := helperDirectory(t)
			statusPath := filepath.Join(directory, "status.json")
			if err := os.Symlink(sentinel, filepath.Join(directory, name)); err != nil {
				t.Fatalf("fixture: plant the link: %v", err)
			}
			// Either the link is refused or the status goes to a real file of the directory; never through the link.
			if err := writeWindowsUninstallStatus(statusPath, status); err == nil {
				info, statErr := os.Lstat(statusPath)
				if statErr != nil || !info.Mode().IsRegular() {
					t.Fatalf("the status file is not a regular file: %v, %v", info, statErr)
				}
				if read, readErr := readWindowsUninstallStatus(statusPath); readErr != nil || read.State != "removing" {
					t.Fatalf("read the status back: %+v, %v", read, readErr)
				}
			}
			untouched(t)
		})
	}
	t.Run("a plan that is a link to an Administrators' file is not read", func(t *testing.T) {
		directory := helperDirectory(t)
		previous := windowsUninstallExecutable
		windowsUninstallExecutable = func() (string, error) { return filepath.Join(directory, "paperboat-uninstall-helper.exe"), nil }
		t.Cleanup(func() { windowsUninstallExecutable = previous })
		planPath := filepath.Join(directory, "plan.json")
		if err := os.Symlink(sentinel, planPath); err != nil {
			t.Fatalf("fixture: plant the link: %v", err)
		}
		if _, err := readWindowsUninstallPlan(planPath); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("a plan behind a link to an Administrators' file: %v, want access denied as the user", err)
		}
	})
}

// Only a held file is waited for; any other removal failure surfaces after a few attempts instead of the full
// five-minute deadline joined with DeadlineExceeded.
func TestWindowsRemovalRetriesOnlyHeldFiles(t *testing.T) {
	for _, err := range []error{windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION, windows.ERROR_DIR_NOT_EMPTY, &fs.PathError{Op: "remove", Path: "x", Err: windows.ERROR_ACCESS_DENIED}} {
		if !retryableWindowsRemoval(err) {
			t.Errorf("%v is not retried", err)
		}
	}
	for _, err := range []error{windows.ERROR_INVALID_NAME, windows.ERROR_DISK_FULL, errors.New("unexpected")} {
		if retryableWindowsRemoval(err) {
			t.Errorf("%v is retried until the deadline", err)
		}
	}
}
