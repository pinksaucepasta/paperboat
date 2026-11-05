//go:build linux

package sessionowner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

// A retiring owner's unit was rendered by the build that installed it, never by
// the build retiring it: pb update to a release that changed the unit text
// refused to retire the previous owner (pbvm1, c09a562c6). Retirement checks the
// declaration's custody, not this build's render.
func TestRetirementAcceptsTheUnitAnEarlierBuildRendered(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned catalog fixture")
	}
	root, err := os.MkdirTemp("/var/lib", "pb-render-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(root, "pb")
	if err = os.WriteFile(pin, image, 0755); err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(pin, "qualification", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if err = os.Mkdir(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	catalog := Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: root, User: "root", Group: "root", Maximum: 4, Runner: &runningOwnerRunner{}}
	old := Descriptor{Schema: Schema, Generation: generation(1), Instance: "render-test", Source: source, Executable: pin, RuntimeRoot: runtimeRoot, InitialStore: true, Endpoint: filepath.Join(root, "old-run", "control.sock"), UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
	ctx := context.Background()
	if err = catalog.Publish(ctx, old); err != nil {
		t.Fatal(err)
	}
	installer, err := catalog.installer(old, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = installer.PublishDefinition(ctx); err != nil {
		t.Fatal(err)
	}
	present, err := verifyRetiredDeclaration(installer, old)
	if err != nil || !present {
		t.Fatalf("this build's own render: present=%v err=%v", present, err)
	}
	// The earlier build rendered one directive this build does not.
	unit, err := os.OpenFile(installer.DefinitionPath(), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unit.WriteString("# a directive an earlier build rendered\n"); err != nil {
		t.Fatal(err)
	}
	if err = unit.Close(); err != nil {
		t.Fatal(err)
	}
	present, err = verifyRetiredDeclaration(installer, old)
	if err != nil || !present {
		t.Fatalf("an earlier build's unit was refused at retirement: present=%v err=%v", present, err)
	}
}
