//go:build darwin || linux

package hostruntimecmd

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/localdaemon"
)

// InheritedInstallServer is the server a pb install with no --server and no build default would use: the saved
// client configuration's, else the installed service declaration's (as installUpdateSource resolves it). It
// reads only; known is false when it cannot tell, so a caller refuses only a definite "no server".
func InheritedInstallServer() (server string, known bool) {
	cfg, err := config.Load("")
	if err != nil {
		return "", false
	}
	if strings.TrimSpace(cfg.ServerURL) != "" {
		return cfg.ServerURL, true
	}
	executable, err := os.Executable()
	if err != nil {
		return "", false
	}
	declared, err := localdaemon.DeclaredServer(executable)
	if err == nil {
		return declared, true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "", true
	}
	return "", false
}
