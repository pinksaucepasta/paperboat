//go:build darwin || linux || windows

package machineguard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuardDomainDiscoveryRestoresLocalAccessAfterUninstall(t *testing.T) {
	state := t.TempDir()
	recordTestDefaultDomain(t, state)
	if err := removeBrowserDomainSelection(state); err != nil {
		t.Fatal(err)
	}
	if _, err := selectedBrowserDomain(state, "1000"); !errors.Is(err, ErrBrowserDomainUnset) {
		t.Fatalf("uninstalled local access: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/client-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"version": "1", "machines_url": "https://console.example.test/machines", "local_access_base_domain": testBrowserDomain,
		}})
	}))
	defer server.Close()
	domain, err := DiscoverBrowserDomain(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordDefaultBrowserDomain(state, domain); err != nil {
		t.Fatal(err)
	}
	if selected, err := selectedBrowserDomain(state, "1000"); err != nil || selected != testBrowserDomain {
		t.Fatalf("reinstalled local access domain = %q, %v", selected, err)
	}
	status := (&guardServer{cfg: Config{StateDir: state}, trustReady: true}).runtimeStatus("1000")
	if status.DefaultBrowserDomain != testBrowserDomain || status.BrowserDomain != testBrowserDomain {
		t.Fatalf("reinstalled local access status = %+v", status)
	}
}
