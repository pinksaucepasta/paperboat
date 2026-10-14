//go:build linux || darwin || windows

package hostinstall

import (
	"runtime"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

func TestInstallationUpdateOriginIsIndependentAndCustomHasNoSource(t *testing.T) {
	previousServer, previousRelease := buildinfo.DefaultServerURL, buildinfo.DefaultReleaseURL
	t.Cleanup(func() { buildinfo.DefaultServerURL, buildinfo.DefaultReleaseURL = previousServer, previousRelease })
	buildinfo.DefaultServerURL = "https://private-control.example.test"
	buildinfo.DefaultReleaseURL = "https://releases.example.test/downloads/"
	source := installsource.Source{Version: "2026.10.14.1", Platform: runtime.GOOS, Architecture: runtime.GOARCH, Distribution: installsource.Official}
	target := InstallationTarget(source)
	if target.RepositoryURL != "https://releases.example.test/downloads/tuf" || bootstrap.VerifyArtifactTarget(target) != nil {
		t.Fatalf("official update target=%+v", target)
	}
	source.Distribution = installsource.Custom
	if target := InstallationTarget(source); target != (bootstrap.ArtifactTarget{}) {
		t.Fatalf("custom build retained an update target: %+v", target)
	}
}
