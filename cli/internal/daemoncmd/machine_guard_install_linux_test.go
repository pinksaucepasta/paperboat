//go:build linux

package daemoncmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/spf13/cobra"
)

// A canceled discovery stops before touching the system guard. On the broken
// command the discovery never happens; an unprivileged install fails instead.
func TestMachineGuardInstallDiscoversMissingDomain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged user to protect the system guard on the pre-fix code")
	}
	// A host with a real guard records a domain and would skip discovery: pretend none is recorded.
	old := machineGuardBrowserDomain
	t.Cleanup(func() { machineGuardBrowserDomain = old })
	machineGuardBrowserDomain = machineguard.DiscoverBrowserDomain
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		cancel()
	}))
	defer server.Close()
	root := &cobra.Command{Use: "pb"}
	root.PersistentFlags().String("config", filepath.Join(t.TempDir(), "config.json"), "")
	root.PersistentFlags().String("server", server.URL, "")
	AddMachineGuardCommand(root)
	root.SetArgs([]string{"machine-guard", "install", "--json"})
	root.SetContext(ctx)
	if err := root.Execute(); err == nil {
		t.Fatal("canceled discovery succeeded")
	}
	if requests.Load() != 1 {
		t.Fatalf("server discovery requests = %d; want 1 before installing the guard", requests.Load())
	}
}
