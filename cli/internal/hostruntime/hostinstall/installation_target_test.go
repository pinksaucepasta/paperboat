//go:build linux || darwin || windows

package hostinstall

import (
	"runtime"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
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

// An Official build installed over a Custom one (which stored an empty target) must carry its own verifiable target:
// reusing the stored one with only the version filled in is what failed on victus (2026-11-19).
func TestInstallationTargetOfOfficialSourceVerifiesWhereTheCustomOneDoesNot(t *testing.T) {
	previousServer, previousRelease := buildinfo.DefaultServerURL, buildinfo.DefaultReleaseURL
	t.Cleanup(func() { buildinfo.DefaultServerURL, buildinfo.DefaultReleaseURL = previousServer, previousRelease })
	buildinfo.DefaultServerURL = "https://private-control.example.test"
	buildinfo.DefaultReleaseURL = "https://releases.example.test/downloads/"
	custom := bootstrap.ArtifactTarget{Version: "2026.11.19.0"}
	if bootstrap.VerifyArtifactTarget(custom) == nil {
		t.Fatal("an artifact rebuilt from a custom install's empty target verified")
	}
	official := installsource.Source{Version: "2026.11.19.0", Platform: runtime.GOOS, Architecture: runtime.GOARCH, Distribution: installsource.Official}
	if err := bootstrap.VerifyArtifactTarget(InstallationTarget(official)); err != nil {
		t.Fatalf("official target: %v", err)
	}
}
