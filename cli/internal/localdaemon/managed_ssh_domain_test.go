//go:build darwin || linux

package localdaemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

func TestManagedSSHUsesDiscoveredDomainAndRefreshesAliases(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client unavailable")
	}
	root, err := os.MkdirTemp("", "pb-domain-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	var domain atomic.Value
	domain.Store("fleet.example.test")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/client-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeManagedSSHTestResponse(w, map[string]any{"version": "1", "machines_url": "http://" + r.Host + "/machines",
			"local_access_base_domain": domain.Load().(string)})
	})
	// The inventory refresh already listed the machines; managed SSH must not list them again.
	mux.HandleFunc("GET /v1/machines", func(w http.ResponseWriter, r *http.Request) {
		t.Error("managed SSH listed the machines again instead of using the inventory's list")
		http.NotFound(w, r)
	})
	machines := []api.UserMachine{{ID: "machine_1", Alias: "build", State: "active", InstallationGeneration: 1,
		SSH: &api.MachineSSH{OSUser: "fixture-user", Port: 38222, TargetGeneration: 1}}}
	server := httptest.NewServer(mux)
	defer server.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := ManagedSSHConfig{ServerURL: server.URL, Auth: &rotatingAuthSource{},
		Store:     config.ProfileStore{Path: filepath.Join(root, "profiles"), Secrets: config.FileSecretStore{Dir: filepath.Join(root, "secrets")}},
		AccountID: "account_domain", CLIClientSessionID: "cli_domain_test", Home: root, RuntimeDirectory: filepath.Join(root, "run"),
		Executable: executable, OwnerUID: uint32(os.Geteuid())}
	runtime, err := StartManagedSSH(t.Context(), cfg, machines)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Error(err)
		}
	})
	clock := time.Now()
	clientConfigurationNow = func() time.Time { return clock }
	t.Cleanup(func() { clientConfigurationNow = time.Now })
	for _, current := range []string{"fleet.example.test", "machines.example.test"} {
		domain.Store(current)
		clock = clock.Add(clientConfigurationTTL) // the cached domain is asked for again
		if err := runtime.Refresh(t.Context(), machines); err != nil {
			t.Fatal(err)
		}
		output, err := exec.CommandContext(t.Context(), ssh, "-G", "-F", filepath.Join(root, ".ssh", "paperboat_config"), "build."+current).Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"port 38222\n", "proxycommand ", "knownhostscommand "} {
			if !strings.Contains(string(output), required) {
				t.Fatalf("discovered domain %s did not select managed SSH option %q", current, required)
			}
		}
	}
}
