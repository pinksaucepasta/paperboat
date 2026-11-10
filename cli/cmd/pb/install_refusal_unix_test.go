//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// F1-07: pb install by a non-admin user fails in the privileged install step and again in the privileged inventory
// that follows it. A plain join of the two has no sole owner and printed "could not finish the command"; the authored
// installation step and recovery action must stay.
func TestNonAdminInstallRefusalShowsTheAuthoredMessageWhenTheInventoryFailsToo(t *testing.T) {
	home := shorttemp.Dir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", home)
	current, _, err := installsource.Current()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(home, "pb-copy")
	if err := os.WriteFile(executable, body, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(executable, buildinfo.Version, buildinfo.Distribution)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errors.New("the privileged child never starts in a workspace test"))
	_, authored := hostruntimecmd.InstallRunningBinary(ctx, executable, source, filepath.Join(home, "bin"), "")
	inventoryFailure := errors.New("sudo: a terminal is required")
	phrases := []string{"supplied Paperboat executable", "administrator access", "pb install"}
	has := func(message string) bool {
		for _, phrase := range phrases {
			if !strings.Contains(message, phrase) {
				return false
			}
		}
		return true
	}
	if has(userFacingError(errors.Join(authored, inventoryFailure))) {
		t.Fatal("the plain join keeps the authored message: this test no longer shows the cause it guards")
	}
	if message := userFacingError(hostruntimecmd.KeepInstallFailureOwner(authored, inventoryFailure)); !has(message) {
		t.Fatalf("the refusal lost its installation step or recovery action: %q", message)
	}
}
