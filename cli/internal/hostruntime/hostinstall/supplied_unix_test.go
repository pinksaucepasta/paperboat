//go:build darwin || linux

package hostinstall

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"os"
	"path/filepath"
	"testing"
)

func TestSuppliedOperationLockExcludesConcurrencyAndRejectsSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.lock")
	first, err := LockSuppliedOperation(path, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	blocked, err := LockSuppliedOperation(path, os.Geteuid())
	if err == nil {
		blocked.Close()
		t.Fatal("concurrent installation acquired lock")
	}
	// A busy lock is the typed busy refusal (the CLI says "retry"), never a bare errno (F14-03 on macOS: errno 35).
	var busy *MaintenanceBusyTimeoutError
	if !errors.Is(err, hostdproto.ErrMaintenanceBusy) || !errors.As(err, &busy) {
		t.Fatalf("busy installation lock = %v, want the typed maintenance-busy refusal", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := LockSuppliedOperation(path, os.Geteuid())
	if err != nil {
		t.Fatal("exited caller left stale lock", err)
	}
	second.Close()
	link := path + ".symlink"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if lock, err := LockSuppliedOperation(link, os.Geteuid()); err == nil {
		lock.Close()
		t.Fatal("followed lock symlink")
	}
}

func TestSuppliedActiveRequestReconcilesProtectedPendingImage(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected transaction journal requires root")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		stage   string
		pending bool
	}{{"prepared", false}, {"prepared", true}, {"binaries_activated", true}, {"services_started", true}} {
		t.Run(tc.stage+map[bool]string{false: "/prior", true: "/pending"}[tc.pending], func(t *testing.T) {
			root := t.TempDir()
			paths := installPaths{worker: filepath.Join(root, "pb"), journal: filepath.Join(root, "journal"), metadata: filepath.Join(root, "metadata")}
			if err := os.WriteFile(paths.worker, append(append([]byte{}, image...), 1), 0700); err != nil {
				t.Fatal(err)
			}
			priorSource, err := installsource.Inspect(paths.worker, "prior", installsource.Custom)
			if err != nil {
				t.Fatal(err)
			}
			pendingSource, err := installsource.Inspect(executable, "pending", installsource.Custom)
			if err != nil {
				t.Fatal(err)
			}
			prior := Request{Source: priorSource, UserMachineID: "retained-machine", InstallationGeneration: 7, StateRoot: "retained-state"}
			journal, err := newInstallJournal(paths, pendingSource)
			if err != nil {
				t.Fatal(err)
			}
			journal.Stage = tc.stage
			if err := writeJournal(paths.journal, journal); err != nil {
				t.Fatal(err)
			}
			if tc.pending {
				if err := os.WriteFile(paths.worker, image, 0700); err != nil {
					t.Fatal(err)
				}
			}
			active, err := suppliedActiveRequest(prior, paths)
			if err != nil {
				t.Fatal(err)
			}
			want := priorSource
			if tc.pending {
				want = pendingSource
			}
			if active.Source != want || active.Executable != paths.worker || active.UserMachineID != prior.UserMachineID || active.InstallationGeneration != prior.InstallationGeneration || active.StateRoot != prior.StateRoot || prior.Source != priorSource {
				t.Fatal("active image or retained enrollment identity changed")
			}
			if err := os.WriteFile(paths.worker, []byte("unrecognized bytes"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := suppliedActiveRequest(prior, paths); err == nil {
				t.Fatal("unrecognized fixed image admitted")
			}
		})
	}
}
