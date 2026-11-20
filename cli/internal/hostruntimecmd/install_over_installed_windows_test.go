//go:build windows

package hostruntimecmd

import (
	"runtime"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
)

// An Official build installed over a dev (Custom) installation, which stored an empty update target, failed "pb TUF target
// descriptor is invalid" because the stored target was reused with only the version filled in (Windows test machine, 2026-11-19).
func TestOfficialInstallOverACustomInstallationCarriesItsOwnArtifact(t *testing.T) {
	previousServer, previousRelease := buildinfo.DefaultServerURL, buildinfo.DefaultReleaseURL
	t.Cleanup(func() { buildinfo.DefaultServerURL, buildinfo.DefaultReleaseURL = previousServer, previousRelease })
	buildinfo.DefaultServerURL = "https://private-control.example.test"
	buildinfo.DefaultReleaseURL = "https://releases.example.test/downloads/"
	custom := hostinstall.WindowsRuntimeConfig{StateRoot: `C:\state`, Workspace: `C:\work`, ControlURL: "https://control.example.test", MachineID: "machine_1", ListenAddress: "127.0.0.1:1"}
	official := installsource.Source{Version: "2026.11.19.0", Platform: "windows", Architecture: runtime.GOARCH, Distribution: installsource.Official}
	request := requestOverInstalled(hostinstall.Request{}, custom, official, `C:\Users\u`, "")
	if err := bootstrap.VerifyArtifactTarget(request.Artifact); err != nil || request.Artifact.Platform != "windows" {
		t.Fatalf("artifact %+v: %v", request.Artifact, err)
	}
	if request.StateRoot != custom.StateRoot || request.UserMachineID != custom.MachineID || request.ControlURL != custom.ControlURL {
		t.Fatalf("installed state was not kept: %+v", request)
	}
}
