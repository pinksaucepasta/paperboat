//go:build windows

package hostruntimecmd

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func TestWindowsPinnedUpdaterUsesProtectedFeatureVersion(t *testing.T) {
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		t.Fatal(err)
	}
	install := hostinstall.WindowsRuntimeConfig{Source: installsource.Source{Version: "2026.08.27.61"},
		OwnerSID:      "S-1-5-21-1-2-3-1001",
		MachineID:     "machine",
		ListenAddress: "127.0.0.1:8080",
		TokenFile:     hostinstall.WindowsHostdTokenPath(),
		StateRoot:     `C:\Users\Pujan\AppData\Local\Paperboat\runtime`,
		Artifact: bootstrap.ArtifactTarget{
			Version:       "2026.08.27.61",
			Architecture:  "amd64",
			RepositoryURL: "https://get.pprbt.dev/tuf",
		},
	}
	config := windowsUpdatedConfigFor(install, layout, "2026.08.27.62")
	if config.ActiveVersion != install.Source.Version {
		t.Fatalf("active feature version=%q want protected source %q", config.ActiveVersion, install.Source.Version)
	}
	if config.RuntimeStateRoot != install.StateRoot {
		t.Fatalf("runtime state root = %q, want persisted owner root %q", config.RuntimeStateRoot, install.StateRoot)
	}
}
