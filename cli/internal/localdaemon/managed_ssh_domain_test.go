//go:build darwin || linux

package localdaemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
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
	mux.HandleFunc("PUT /v1/ssh/client-keys/{fingerprint}", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			PublicKey string `json:"public_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeManagedSSHTestResponse(w, api.ManagedSSHClientKey{Type: "client_key", Version: 1,
			Fingerprint: r.PathValue("fingerprint"), PublicKey: input.PublicKey, State: "active", ReconciliationVersion: 1})
	})
	mux.HandleFunc("GET /v1/client-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeManagedSSHTestResponse(w, map[string]any{"version": "1", "machines_url": "http://" + r.Host + "/machines",
			"local_access_base_domain": domain.Load().(string)})
	})
	mux.HandleFunc("GET /v1/machines", func(w http.ResponseWriter, r *http.Request) {
		writeManagedSSHTestResponse(w, api.UserMachinePage{Items: []api.UserMachine{{ID: "machine_1", Alias: "build", State: "active", InstallationGeneration: 1}}, Pagination: api.Pagination{Limit: 200, Total: 1}})
	})
	mux.HandleFunc("GET /v1/machines/machine_1/ssh-target", func(w http.ResponseWriter, r *http.Request) {
		writeManagedSSHTestResponse(w, api.ManagedSSHTarget{Type: "machine_target", Version: 1, MachineID: "machine_1",
			MachineGeneration: 1, OSUser: "fixture-user", Port: 38222, ReconciliationVersion: 1})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := StartManagedSSH(t.Context(), ManagedSSHConfig{ServerURL: server.URL, Auth: &rotatingAuthSource{},
		Store:              config.ProfileStore{Path: filepath.Join(root, "profiles"), Secrets: config.FileSecretStore{Dir: filepath.Join(root, "secrets")}},
		CLIClientSessionID: "cli_domain_test", Home: root, RuntimeDirectory: filepath.Join(root, "run"),
		Executable: executable, OwnerUID: uint32(os.Geteuid())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, current := range []string{"fleet.example.test", "machines.example.test"} {
		domain.Store(current)
		if err := runtime.Refresh(t.Context()); err != nil {
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
