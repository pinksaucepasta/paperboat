//go:build linux || darwin

package hostinstall

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"
)

// First enrollment must expose the already-activated protected payload to
// updater readiness without inventing a committed source or accepting unsafe
// receipts. Pending executing identity never becomes committed metadata.
func TestPendingInstalledSourceUsesProtectedNativeReceipt(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected native source custody")
	}
	for _, mode := range []string{"activated", "services started", "not activated", "previous metadata", "tampered payload", "symlink receipt", "unsafe directory", "foreign payload", "committed metadata", "unbound metadata", "enrolled replacement", "committed before receipt cleanup", "prior identity mismatch"} {
		t.Run(mode, func(t *testing.T) {
			request := validRequest(t)
			account, err := user.LookupId("0")
			if err != nil {
				t.Fatal(err)
			}
			home, err := filepath.EvalSymlinks(account.HomeDir)
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.MkdirTemp(home, "paperboat-pending-source-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			paths := installPaths{root: root, worker: filepath.Join(root, "pb"), metadata: filepath.Join(root, "metadata"), journal: filepath.Join(root, "install-journal")}
			image, err := os.ReadFile(request.Executable)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(paths.worker, image, 0755); err != nil {
				t.Fatal(err)
			}
			journal := installJournal{Schema: journalSchemaV1, Stage: "binaries_activated", MetadataCaptured: true, Source: request.Source, UpdatedAt: time.Now().UTC()}
			allowed := mode == "activated" || mode == "services started" || mode == "committed metadata" || mode == "unbound metadata" || mode == "enrolled replacement" || mode == "committed before receipt cleanup"
			switch mode {
			case "services started":
				journal.Stage = "services_started"
			case "not activated":
				journal.Stage = "prepared"
			case "previous metadata":
				journal.PreviousMetadata = []byte("previous identity")
			case "tampered payload":
				if err = os.WriteFile(paths.worker, []byte("tampered"), 0755); err != nil {
					t.Fatal(err)
				}
			case "foreign payload":
				if err = os.Chown(paths.worker, 1001, -1); err != nil {
					t.Fatal(err)
				}
			case "committed metadata", "unbound metadata":
				request.Executable = paths.worker
				request.EnrollmentPending = mode == "unbound metadata"
				if err = writeInstallMetadata(paths.metadata, request); err != nil {
					t.Fatal(err)
				}
			case "enrolled replacement", "committed before receipt cleanup", "prior identity mismatch":
				previous := request
				previous.Executable = paths.worker
				previous.Source.Version = "previous"
				if err = writeInstallMetadata(paths.metadata, previous); err != nil {
					t.Fatal(err)
				}
				journal, err = newInstallJournal(paths, request.Source)
				if err != nil {
					t.Fatal(err)
				}
				journal.Stage = "binaries_activated"
				if mode == "committed before receipt cleanup" {
					journal.Stage = "services_started"
					request.Executable = paths.worker
					if err = writeInstallMetadata(paths.metadata, request); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "prior identity mismatch" {
					previous.UID++
					journal.PreviousMetadata, err = json.Marshal(previous)
					if err != nil {
						t.Fatal(err)
					}
				}

			}
			if err = writeJournal(paths.journal, journal); err != nil {
				t.Fatal(err)
			}
			if mode == "committed metadata" || mode == "unbound metadata" {
				if err = os.Remove(paths.journal); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink receipt" {
				target := filepath.Join(root, "receipt-target")
				if err = os.Rename(paths.journal, target); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(target, paths.journal); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "unsafe directory" {
				if err = os.Chmod(root, 0777); err != nil {
					t.Fatal(err)
				}
			}
			source, committed, err := pendingInstalledSource(paths, request.UID)
			if !allowed {
				if err == nil {
					t.Fatal("unsafe or unactivated source accepted")
				}
				return
			}
			if err != nil || source != request.Source || committed != (mode == "committed metadata") {
				t.Fatalf("readiness source/commit ownership changed: committed=%v error=%v", committed, err)
			}
			if _, err = os.Lstat(paths.journal); err != nil && mode != "committed metadata" && mode != "unbound metadata" {
				t.Fatal("readiness consumed installation receipt", err)
			}
		})
	}
}
