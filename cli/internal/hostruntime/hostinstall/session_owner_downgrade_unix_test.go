//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/sessionowner"
)

// pb update --from an older build was refused with "session owner identity or custody is invalid": after the newer
// build retired the older one's owner, the catalogue kept it as an archive row (its history) that names the newer
// build's code under the older generation. Reinstalling the older build found a same-generation row that did not
// name its artifact. The archive row is the retired owner's history, not a rival: the older build takes it back.
func TestReinstallingAnOlderBuildTakesBackItsArchivedSessionOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected native catalog custody")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, "pb-owner-downgrade-")
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
	pins := map[string]string{"older": filepath.Join(root, "older-pin"), "newer": filepath.Join(root, "newer-pin")}
	if err = os.WriteFile(pins["older"], image, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pins["newer"], append(append([]byte(nil), image...), 0), 0755); err != nil {
		t.Fatal(err)
	}
	older, err := installsource.Inspect(pins["older"], "older-version", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := installsource.Inspect(pins["newer"], "newer-version", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if err = os.Mkdir(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	c := sessionowner.Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: root, User: "root", Group: "root", Maximum: 4, Runner: absentUnits{}}
	owner := func(generation string, source installsource.Source, pin string, initial bool) sessionowner.Descriptor {
		return sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: generation, Instance: "u0", Source: source, Executable: pin, RuntimeRoot: runtimeRoot, InitialStore: initial, Endpoint: filepath.Join(root, generation[:8]+".sock"), UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
	}
	olderGeneration, newerGeneration := older.SHA256[:32], newer.SHA256[:32]
	ctx := context.Background()
	// What Coordinator.recordRetired leaves after the newer build retired the older owner that still held history.
	archive := owner(olderGeneration, newer, pins["newer"], true)
	for _, d := range []sessionowner.Descriptor{archive, owner(newerGeneration, newer, pins["newer"], false)} {
		if err = c.Publish(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	desired := owner(olderGeneration, older, pins["older"], false)
	bound, err := bindInitialSessionStore(ctx, c, desired, newerGeneration)
	if err != nil {
		t.Fatalf("the older build could not take back its archived owner: %v", err)
	}
	if bound.Source != older || bound.Executable != pins["older"] || !bound.InitialStore {
		t.Fatalf("the older build's owner is not its own code and history: %+v", bound)
	}
	if err = c.Publish(ctx, bound); err != nil {
		t.Fatalf("publishing the older build's owner: %v", err)
	}
	// A different build's owner is never taken over: only a row that names the active build's code is an archive.
	other := owner(olderGeneration, older, pins["older"], true)
	other.Source.Version = "another-version"
	if _, err = bindInitialSessionStore(ctx, c, other, newerGeneration); err == nil {
		t.Fatal("a same-generation owner of the same code under another version was replaced")
	}
}

// absentUnits reports every native unit as not installed: the archive row has no process.
type absentUnits struct{}

func (absentUnits) Run(context.Context, string, ...string) error {
	return errors.New("unexpected native mutation")
}
func (absentUnits) Output(context.Context, string, ...string) (string, error) {
	return "LoadState=not-found\n", nil
}
