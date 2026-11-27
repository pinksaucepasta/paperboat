//go:build windows

package localapi

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/localtestroot"
)

// A test binary with the local root set never resolves the installed daemon's state, DNS file or pipe.
func TestTestLocalRootMovesStateDNSAndPipeAwayFromTheInstalledDaemon(t *testing.T) {
	t.Setenv(localtestroot.EnvName, "")
	real, err := CurrentPaths(0)
	if err != nil {
		t.Fatal(err)
	}
	realDNS := DNSFile()
	root := filepath.Clean(t.TempDir())
	t.Setenv(localtestroot.EnvName, root)
	isolated, err := CurrentPaths(0)
	if err != nil {
		t.Fatal(err)
	}
	if isolated.StateRoot != filepath.Join(root, "Paperboat", "state") || isolated.StateRoot == real.StateRoot {
		t.Fatalf("state root=%q real=%q", isolated.StateRoot, real.StateRoot)
	}
	if isolated.SocketPath == real.SocketPath || !strings.HasPrefix(isolated.SocketPath, `\\.\pipe\paperboat-local-api-`) || len(isolated.SocketPath) != len(real.SocketPath) {
		t.Fatalf("pipe=%q real=%q", isolated.SocketPath, real.SocketPath)
	}
	if dns := DNSFile(); dns == realDNS || !strings.HasPrefix(dns, root) {
		t.Fatalf("dns=%q real=%q", dns, realDNS)
	}
	t.Setenv(localtestroot.EnvName, "relative")
	if relative, err := CurrentPaths(0); err != nil || relative != real {
		t.Fatalf("a relative root was honored: %+v %v", relative, err)
	}
}
