//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
)

func TestSuppliedInstallFailureKeepsSafeStepAndPrivateCause(t *testing.T) {
	home, err := os.MkdirTemp("", "pb-refusal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", home)
	executable, source, err := installsource.Current()
	if err != nil {
		t.Fatal(err)
	}
	private := errors.New("PRIVATE installation supervisor failure")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(private) // The real privileged child must never start in a workspace test.
	_, failure := hostruntimecmd.InstallRunningBinary(ctx, executable, source, filepath.Join(home, "bin"))
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
