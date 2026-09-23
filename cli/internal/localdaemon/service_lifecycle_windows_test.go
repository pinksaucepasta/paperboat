//go:build windows

package localdaemon

import (
	"context"
	"strings"
	"testing"
)

func TestUninstallCurrentUserServiceRefusesManagedInstallerOwnership(t *testing.T) {
	err := UninstallCurrentUserService(context.Background(), `C:\Program Files\Paperboat\pb.exe`)
	if err == nil || !strings.Contains(err.Error(), "Paperboat uninstaller") {
		t.Fatalf("uninstall error=%v", err)
	}
}
