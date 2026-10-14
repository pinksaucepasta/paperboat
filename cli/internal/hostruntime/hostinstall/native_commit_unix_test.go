//go:build darwin || linux

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

func TestNativeCommitRetainsRollbackUntilMetadataCommit(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected install transaction requires root")
	}
	root := t.TempDir()
	paths := installPaths{worker: filepath.Join(root, "pb"), workerRollback: filepath.Join(root, "rollback"), workerPrevious: filepath.Join(root, "previous"), workerNext: filepath.Join(root, "next"), metadata: filepath.Join(root, "metadata"), journal: filepath.Join(root, "journal"), updateState: filepath.Join(root, "updates")}
	for path, body := range map[string]string{paths.worker: "new bytes", paths.workerRollback: "old bytes", paths.metadata: "old protected metadata"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	source := installsource.Source{Version: "custom"}
	journal, err := newInstallJournal(paths, source)
	if err != nil {
		t.Fatal(err)
	}
	journal.Stage = "services_started"
	if err = writeJournal(paths.journal, journal); err != nil {
		t.Fatal(err)
	}
	if err = replacePrevious(paths.workerRollback, paths.workerPrevious); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(paths.workerRollback); err != nil {
		t.Fatal("rollback bytes retired before metadata commit", err)
	}
	if err = writeInstallMetadata(paths.metadata, Request{Source: source}); err != nil {
		t.Fatal(err)
	}
	// A later commit failure must restore both binary and protected declaration.
	if err = rollbackFiles(paths, journal); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{paths.worker: "old bytes", paths.metadata: "old protected metadata"} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, []byte(want)) {
			t.Fatalf("rollback did not restore %s: %v", filepath.Base(path), err)
		}
	}
}

// Both supplied (already locked) and ordinary native commits must persist the
// candidate identity and retire pending installation state before reporting success.
func TestNativeCommitFinishesWithExistingUpdateState(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected install transaction requires root")
	}
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("nativeLockHeld=%t", held), func(t *testing.T) {
			root := t.TempDir()
			paths := installPaths{worker: filepath.Join(root, "pb"), workerRollback: filepath.Join(root, "rollback"), workerPrevious: filepath.Join(root, "previous"), workerNext: filepath.Join(root, "next"), metadata: filepath.Join(root, "metadata"), journal: filepath.Join(root, "journal"), updateState: filepath.Join(root, "updates")}
			for path, body := range map[string]string{paths.worker: "new bytes", paths.workerRollback: "old bytes", paths.metadata: "old metadata"} {
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(paths.updateState, 0700); err != nil {
				t.Fatal(err)
			}
			lock, err := updated.LockUnixNativeInstall(context.Background(), paths.updateState)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := updated.PrepareUnixNativeInstall(paths.updateState); err != nil {
				t.Fatal(err)
			}
			if !held {
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
			}
			source := installsource.Source{Version: "custom"}
			request := Request{Schema: SchemaV1, Source: source}
			journal, err := newInstallJournal(paths, source)
			if err != nil {
				t.Fatal(err)
			}
			journal.Stage = "services_started"
			if err := writeJournal(paths.journal, journal); err != nil {
				t.Fatal(err)
			}
			if err := commitPreparedInstallationLocked(context.Background(), request, paths, held); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(paths.metadata)
			if err != nil {
				t.Fatal(err)
			}
			var persisted Request
			if err := json.Unmarshal(body, &persisted); err != nil || persisted.Schema != SchemaV1 || persisted.Source != source {
				t.Fatalf("candidate metadata not committed: %v", err)
			}
			for _, path := range []string{paths.journal, paths.workerRollback, filepath.Join(paths.updateState, "native-install.json")} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("pending state retained at %s: %v", filepath.Base(path), err)
				}
			}
			previous, err := os.ReadFile(paths.workerPrevious)
			if err != nil || string(previous) != "old bytes" {
				t.Fatalf("previous image missing: %v", err)
			}
		})
	}
}
