//go:build windows

package hostruntimecmd

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"os"
)

func ConfigureBrowserDomain(ctx context.Context, domain string) (resultErr error) {
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
	defer func() {
		if err := cleanup(); err != nil {
			resultErr = withCleanup("browser domain approval or its cleanup failed", resultErr, err)
		}
	}()
	return elevation.RunRuntimeService(ctx, staged, elevation.ActionBrowserDomain, BrowserDomainRequest{Owner: owner, Domain: domain})
}
