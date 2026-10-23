//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
)

func TestSuppliedInstallFailureKeepsSafeStepAndPrivateCause(t *testing.T) {
	home := shorttemp.Dir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", home)
	// The test binary is group-writable under umask 0002 and the installer now
	// refuses that before any privileged work, so supply an owner-only copy.
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
	private := errors.New("PRIVATE installation supervisor failure")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(private) // The real privileged child must never start in a workspace test.
	_, failure := hostruntimecmd.InstallRunningBinary(ctx, executable, source, filepath.Join(home, "bin"), "")
	if !errors.Is(failure, private) {
		t.Fatal("installer lost its actual diagnostic cause")
	}
	message := userFacingError(failure)
	if !strings.Contains(message, "supplied Paperboat executable") || !strings.Contains(message, "administrator access") || !strings.Contains(message, "pb install") || strings.Contains(message, "PRIVATE") {
		t.Fatal("CLI lost the authored installation step or exposed a diagnostic cause")
	}
	if classifyCommandFailure(failure).kind != commandUnexpected {
		t.Fatal("presentation hid an unexpected diagnostic cause")
	}
}
