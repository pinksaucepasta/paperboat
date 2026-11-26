//go:build windows

package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	hostruntimeservice "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/cli/internal/windowsopenssh"
	"golang.org/x/sys/windows"
)

// After `pb update --from` of the installed bytes the new worker runs from versions\custom-<sha>, while the SSH
// generation of those bytes still names the first install's path: SSH stayed refused until another build.
func TestWindowsSSHGenerationBindsTheSameBytesFromAnotherPinnedPath(t *testing.T) {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	layout, err := hostruntimeservice.WindowsUserLayout(sid)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := installsource.Inspect(executable, "2026.1.1.1", installsource.Official)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := hostruntimeservice.WindowsUserInstance(sid)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := windowsopenssh.GenerationDescriptor{Schema: "paperboat.windows-ssh-generation/v1", Instance: instance,
		Generation: prepared.SHA256[:32], OwnerSID: sid, Source: prepared, Port: 40000,
		Executable: filepath.Join(layout.ReleasesRoot, "versions", prepared.Version, "pb.exe")}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("descriptor fixture: %v", err)
	}
	supplied := prepared
	supplied.Distribution, supplied.AutomaticUpdates = installsource.Custom, false
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	// The worker's own layout is under Program Files; the pinned paths are checked below a stand-in releases root.
	releases := t.TempDir()
	place := func(image []byte, elements ...string) string {
		path := filepath.Join(append([]string{releases}, elements...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, image, 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	official := place(image, "versions", prepared.Version, "pb.exe")
	custom := place(image, "versions", "custom-"+prepared.SHA256, "pb.exe")
	for name, c := range map[string]struct {
		source     installsource.Source
		executable string
	}{"first install": {prepared, official}, "same bytes supplied again": {supplied, custom}, "signed bytes run from the supplied path": {prepared, custom}} {
		if err := bindExecutingWindowsSSHGeneration(releases, descriptor, c.source, c.executable); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := bindExecutingWindowsSSHGeneration(releases, descriptor, prepared, place(image, "pb.exe")); err == nil {
		t.Error("a copy outside the pinned paths bound the generation")
	}
	other := place(append(image, 0), "versions", "custom-"+prepared.SHA256, "pb.exe")
	if err := bindExecutingWindowsSSHGeneration(releases, descriptor, prepared, other); err == nil {
		t.Error("a worker running other bytes bound the generation")
	}
	changed, err := installsource.Inspect(other, prepared.Version, installsource.Official)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindExecutingWindowsSSHGeneration(releases, descriptor, changed, other); err == nil {
		t.Error("another artifact bound the generation")
	}
}
