//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
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
		if err := retryWindowsRemoval(ctx, profile, state, nil); err != nil {
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
		if err := retryWindowsRemoval(ctx, profile, state, []string{inbox}); err != nil {
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
			if err := retryWindowsRemoval(ctx, profile, path, nil); !errors.Is(err, errWindowsUserStateRefused) {
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
			if err := retryWindowsRemoval(ctx, profile, path, nil); !errors.Is(err, errWindowsUserStateRefused) {
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
	err = retryWindowsRemoval(ctx, profile, state, nil)
	if !planted {
		t.Fatal("fixture: the junction was never planted")
	}
	if body, readErr := os.ReadFile(sentinel); readErr != nil || string(body) != "not Paperboat's" {
		t.Fatalf("the administrators' file behind the junction was deleted with the helper's rights: %v", readErr)
	}
	// The first attempt is denied as the user; the next check sees the junction and refuses the path.
	if !errors.Is(err, errWindowsUserStateRefused) {
		t.Fatalf("removal through a junction planted after the check: %v, want the refusal", err)
	}
}
