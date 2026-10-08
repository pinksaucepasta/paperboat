//go:build windows

package updated

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func TestWindowsFeatureTerminalPathsRestoreNativeUpdaterExactlyOnce(t *testing.T) {
	for _, stage := range []windowsActivationStage{windowsActivationCommitted, windowsActivationRolledBack} {
		t.Run(string(stage), func(t *testing.T) {
			j := windowsActivationJournal{Stage: stage, Version: "2026.10.08.32", PreviousVersion: "2026.10.08.31"}
			want := j.Version
			if stage == windowsActivationRolledBack {
				want = j.PreviousVersion
			}
			starts, verifies, retires := 0, 0, 0
			running := false
			restore := func(ctx context.Context, j windowsActivationJournal) error {
				return restoreWindowsFeatureUpdater(ctx, j, func(context.Context, windowsActivationJournal) error { verifies++; return nil }, func(context.Context) error { starts++; running = true; return nil }, func(context.Context) (ControlResponse, error) {
					if !running {
						return ControlResponse{}, errors.New("control listener absent")
					}
					// Native pin stays .31 even after feature .32 commits.
					return ControlResponse{Version: want, UpdaterVersion: "2026.10.08.31"}, nil
				})
			}
			var activationErr error
			if stage == windowsActivationRolledBack {
				activationErr = errors.New("candidate failed; recovered")
			}
			retire := func() error { retires++; return nil }
			if err := finishWindowsActivatorResult(context.Background(), j, activationErr, restore, retire); err != nil {
				t.Fatal(err)
			}
			// Recovered terminal journals run only final ownership restoration. A lost
			// retirement acknowledgement must not restart an already usable updater.
			if err := finishWindowsActivatorResult(context.Background(), j, activationErr, restore, retire); err != nil {
				t.Fatal(err)
			}
			if starts != 1 || verifies != 2 || retires != 2 {
				t.Fatalf("starts=%d verifies=%d retires=%d", starts, verifies, retires)
			}
		})
	}
}

func TestWindowsFeatureUpdaterFailureRetainsActivatorForSafeRetry(t *testing.T) {
	j := windowsActivationJournal{Stage: windowsActivationCommitted, Version: "2026.10.08.32", PreviousVersion: "2026.10.08.31"}
	starts, retires := 0, 0
	lost := errors.New("start acknowledgement lost")
	running := false
	restore := func(ctx context.Context, j windowsActivationJournal) error {
		return restoreWindowsFeatureUpdater(ctx, j, func(context.Context, windowsActivationJournal) error { return nil }, func(context.Context) error { starts++; running = true; return lost }, func(context.Context) (ControlResponse, error) {
			if running {
				return ControlResponse{Version: j.Version}, nil
			}
			return ControlResponse{}, errors.New("stopped")
		})
	}
	retire := func() error { retires++; return nil }
	if err := finishWindowsActivatorResult(context.Background(), j, nil, restore, retire); !errors.Is(err, lost) {
		t.Fatalf("err=%v", err)
	}
	if retires != 0 {
		t.Fatal("retired activator before confirmed updater readiness")
	}
	if err := finishWindowsActivatorResult(context.Background(), j, nil, restore, retire); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || retires != 1 {
		t.Fatalf("starts=%d retires=%d", starts, retires)
	}
}

func TestWindowsFeatureUpdaterRejectsWrongNativePinBeforeStart(t *testing.T) {
	layout := service.Layout{ReleasesRoot: filepath.Join(t.TempDir(), "releases")}
	pin := filepath.Join(layout.ReleasesRoot, "versions", "2026.10.08.31", "pb.exe")
	expected := windowsServiceTarget{Executable: pin, SHA256: strings.Repeat("a", 64), Length: 100, Arguments: []string{"daemon", "__runtime-updated", "--instance", "owner"}}
	identity := service.WindowsExecutableIdentity{Executable: pin, SHA256: expected.SHA256, Length: 100}
	if err := validateWindowsFeatureNativePin(layout, expected, expected, identity); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"scm_path", "declaration_path", "hash", "length", "arguments"} {
		t.Run(kind, func(t *testing.T) {
			actual := expected
			trusted := identity
			switch kind {
			case "scm_path":
				actual.Executable = filepath.Join(layout.ReleasesRoot, "versions", "2026.10.08.32", "pb.exe")
			case "declaration_path":
				trusted.Executable = filepath.Join(layout.ReleasesRoot, "versions", "2026.10.08.32", "pb.exe")
			case "hash":
				trusted.SHA256 = strings.Repeat("b", 64)
			case "length":
				trusted.Length++
			case "arguments":
				actual.Arguments = []string{"daemon", "__runtime-hostd", "--instance", "owner"}
			}
			starts, statuses := 0, 0
			j := windowsActivationJournal{Stage: windowsActivationCommitted, Version: "2026.10.08.32"}
			err := restoreWindowsFeatureUpdater(context.Background(), j, func(context.Context, windowsActivationJournal) error {
				return validateWindowsFeatureNativePin(layout, expected, actual, trusted)
			}, func(context.Context) error { starts++; return nil }, func(context.Context) (ControlResponse, error) {
				statuses++
				return ControlResponse{Version: j.Version}, nil
			})
			if !errors.Is(err, errInvalidWindowsActivation) || starts != 0 || statuses != 0 {
				t.Fatalf("err=%v starts=%d status=%d", err, starts, statuses)
			}
		})
	}
}

func TestWindowsFeatureUpdaterFinalizationLeavesNativeMaintenanceAndPendingTransactionsAlone(t *testing.T) {
	for _, stage := range []windowsActivationStage{windowsActivationStaged, windowsActivationSwitching, windowsActivationServicesLive, windowsActivationCommitReady} {
		restores, retires := 0, 0
		cause := errors.New("still pending")
		err := finishWindowsActivatorResult(context.Background(), windowsActivationJournal{Stage: stage}, cause, func(context.Context, windowsActivationJournal) error { restores++; return nil }, func() error { retires++; return nil })
		if !errors.Is(err, cause) || restores != 0 || retires != 0 {
			t.Fatalf("stage=%s err=%v restores=%d retires=%d", stage, err, restores, retires)
		}
	}
	restores, retires := 0, 0
	j := windowsActivationJournal{Stage: windowsActivationCommitted}
	j.Release.SupervisorMaintenance = true
	if err := finishWindowsActivatorResult(context.Background(), j, nil, func(context.Context, windowsActivationJournal) error { restores++; return nil }, func() error { retires++; return nil }); err != nil {
		t.Fatal(err)
	}
	if restores != 0 || retires != 1 {
		t.Fatalf("restores=%d retires=%d", restores, retires)
	}
}
