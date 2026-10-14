//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"os"
	"path/filepath"
	"testing"
)

// Periodic reconciliation runs inside updated itself. An unrelated native
// service or install marker must not be stopped/rewritten during carrier-only
// rollback when the protected installed source stayed unchanged.
func TestCarrierOnlyRollbackPreservesInstalledRuntime(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected installation metadata requires root")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(executable, "qualification", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	paths := installPaths{instance: "u2147483000", worker: filepath.Join(root, "pb"), workerNext: filepath.Join(root, "next"), metadata: filepath.Join(root, "metadata"), journal: filepath.Join(root, "install-journal"), updateState: filepath.Join(root, "update-state")}
	if err := os.WriteFile(paths.worker, image, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"protected":"unchanged installed identity"}`)
	if err := os.WriteFile(paths.metadata, metadata, 0600); err != nil {
		t.Fatal(err)
	}
	marker := []byte("existing activation marker")
	if err := os.WriteFile(paths.journal, marker, 0600); err != nil {
		t.Fatal(err)
	}
	baseline := unixCarrierBaseline{CarrierOnly: true, Worker: &source, Metadata: metadata}
	body, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	state := unixCarrierInstallation{request: Request{Source: source}, paths: paths}
	if err := state.Restore(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if err := source.Verify(paths.worker); err != nil {
		t.Fatal("installed runtime changed", err)
	}
	for path, want := range map[string][]byte{paths.metadata: metadata, paths.journal: marker} {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, want) {
			t.Fatalf("carrier rollback changed installed state: %v", err)
		}
	}
}
