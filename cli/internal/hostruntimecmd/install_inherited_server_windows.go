//go:build windows

package hostruntimecmd

import (
	"os"
	"strings"

	clientconfig "github.com/pinksaucepasta/paperboat/cli/internal/config"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
)

// InheritedInstallServer is the server a pb install with no --server and no build default would use: the saved
// client configuration's, else the existing installation's. It reads only; known is false when it cannot tell
// (the install itself then reports the problem), so a caller refuses only a definite "no server".
func InheritedInstallServer() (server string, known bool) {
	if client, err := clientconfig.Load(""); err != nil {
		return "", false
	} else if strings.TrimSpace(client.ServerURL) != "" {
		return client.ServerURL, true
	}
	sid, err := currentBootstrapSID()
	if err != nil {
		return "", false
	}
	instance, err := hostinstall.WindowsInstanceForSID(sid)
	if err != nil {
		return "", false
	}
	existing, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err == nil {
		return existing.ControlURL, true
	}
	if runtimeCausesOnly(err, os.ErrNotExist, hostinstall.ErrNotInstalled) {
		return "", true
	}
	return "", false
}
