//go:build linux

package hostinstall

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

// pbvm1 F14-04: after a successful update the previous build's pin stayed with nothing running from it. The
// install's own inventory ran while the previous generation drained, and the updater's sweep kept every pin younger
// than a 10-minute window. The updater's carrier reconciliation, which holds the native installation lock an install
// holds for its whole run, now removes an unreferenced pin, however new, in its pass.
func TestDrainedGenerationPinIsSweptWithoutAnotherInstall(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected root-owned pin fixture")
	}
	root, err := os.MkdirTemp("/var/lib", "pb-pin-sweep-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err = os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := installPaths{root: root, workerRollback: filepath.Join(root, "releases", "pb.rollback"), instance: "u0"}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	pin := func(body []byte) (installsource.Source, string) {
		t.Helper()
		staged := filepath.Join(root, "staged")
		if err := os.WriteFile(staged, body, 0o755); err != nil {
			t.Fatal(err)
		}
		source, err := installsource.Inspect(staged, "pin", installsource.Custom)
		if err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(root, "releases", "pins", source.SHA256)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(staged, filepath.Join(directory, "pb")); err != nil {
			t.Fatal(err)
		}
		return source, filepath.Join(directory, "pb")
	}
	active, activePin := pin(image)
	_, previousPin := pin(append(append([]byte(nil), image...), 0)) // the build the update replaced, minutes old
	catalog := sessionowner.Catalog{Root: filepath.Join(root, "session-owners"), ConfigRoot: root, User: "root", Group: "root", Maximum: 4, Runner: &withdrawRunner{}}
	runtimeRoot := filepath.Join(root, "runtime")
	if err = os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: active.SHA256[:32], Instance: paths.instance, Source: active,
		Executable: activePin, RuntimeRoot: runtimeRoot, InitialStore: true, Endpoint: filepath.Join(root, "owner", "control.sock"),
		UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
	ctx := context.Background()
	if err = catalog.Publish(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err = pruneSessionOwnerPinsAt(ctx, catalog, paths, 0, runtimeRoot, active.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(previousPin); !os.IsNotExist(err) {
		t.Fatalf("the drained generation's pin survived the sweep: %v", err)
	}
	if _, err = os.Lstat(activePin); err != nil {
		t.Fatalf("the active pin was removed: %v", err)
	}
}
