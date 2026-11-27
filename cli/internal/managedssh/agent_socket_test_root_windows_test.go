//go:build windows

package managedssh

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/localtestroot"
)

// The managed SSH agent pipe of a test binary with the local root set is never the installed daemon's.
func TestTestLocalRootMovesTheAgentPipeAwayFromTheInstalledDaemon(t *testing.T) {
	t.Setenv(localtestroot.EnvName, "")
	real, err := ownerAgentSocket("")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(localtestroot.EnvName, filepath.Clean(t.TempDir()))
	isolated, err := ownerAgentSocket("")
	if err != nil {
		t.Fatal(err)
	}
	if isolated == real || !strings.HasPrefix(isolated, `\\.\pipe\paperboat-ssh-agent-`) || !validWindowsAgentPipe(isolated) {
		t.Fatalf("pipe=%q real=%q", isolated, real)
	}
}
