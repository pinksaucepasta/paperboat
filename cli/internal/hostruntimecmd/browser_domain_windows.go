//go:build windows

package hostruntimecmd

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"os"
)

func ConfigureBrowserDomain(ctx context.Context, domain string) error {
	owner, err := currentBootstrapSID()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	staged, cleanup, err := stageWindowsElevationExecutable(executable)
	if err != nil {
		return err
	}
	defer cleanup()
	return elevation.RunRuntimeService(ctx, staged, elevation.ActionBrowserDomain, BrowserDomainRequest{Owner: owner, Domain: domain})
}
