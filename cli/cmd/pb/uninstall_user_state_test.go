package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	documentation "github.com/pinksaucepasta/paperboat/docs"
)

func TestUserStateIsRetainedWhenSystemPurgeIsNotConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		daemonStopped bool
		runtimePurged bool
	}{
		{name: "root purge failed", daemonStopped: true, runtimePurged: false},
		{name: "daemon stop unconfirmed", daemonStopped: false, runtimePurged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			purged := false
			err := removeUserStateAfterSystemPurge(tc.daemonStopped, tc.runtimePurged, func() error {
				purged = true
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "retained") {
				t.Fatalf("error = %v, want retained user state", err)
			}
			if purged {
				t.Fatal("user state was removed although system removal was not confirmed")
			}
		})
	}
}

func TestUserStateRemovalRunsOnlyAfterConfirmedSystemPurge(t *testing.T) {
	purged := false
	if err := removeUserStateAfterSystemPurge(true, true, func() error { purged = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !purged {
		t.Fatal("confirmed system removal did not reach user state removal")
	}
	wantErr := errors.New("keyring unavailable")
	if err := removeUserStateAfterSystemPurge(true, true, func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("user state error = %v, want %v", err, wantErr)
	}
}

func TestRemoveInstalledManualsTargetsInstallShareDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("manual pages are removed only on linux; darwin keeps the shared PKG pages")
	}
	home := t.TempDir()
	manuals := filepath.Join(home, ".local", "share", "man")
	if err := documentation.Install(manuals); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(manuals, "man1"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("installed manuals missing: %v", err)
	}
	foreign := filepath.Join(manuals, "man1", "unrelated.1")
	if err := os.WriteFile(foreign, []byte("not paperboat"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeInstalledManuals(home); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, err := os.Lstat(filepath.Join(manuals, "man1", entry.Name())); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("installed manual %s still exists (err=%v)", entry.Name(), err)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("unowned manual was removed: %v", err)
	}
}
