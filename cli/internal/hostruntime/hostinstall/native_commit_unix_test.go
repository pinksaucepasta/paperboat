//go:build darwin || linux

package hostinstall

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
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
