package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
)

func TestClientConfigurationPublishesDashboardURLs(t *testing.T) {
	cfg := config.Default()
	cfg.LocalAccessBaseDomain = "local.example.test"
	cfg.RuntimeBaseDomain = "runtime.example.test"
	cfg.ReleaseBaseURL = "https://get.example.test"
	cfg.HTTP.BrowserLoginOrigin = "https://console.example.test"
	cfg.CLIAuth.VerificationURL = "https://dashboard.paperboat.test/cli/authorize"
	cfg.CLIAuth.MachinesURL = "https://console.paperboat.test/machines/add"
	request := httptest.NewRequest(http.MethodGet, "/v1/client-configuration", nil)
	recorder := httptest.NewRecorder()

	clientConfiguration(cfg).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data clientConfigurationResponse `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Version != "1" {
		t.Fatalf("version=%q", response.Data.Version)
	}
	if response.Data.CLIVerificationURL != cfg.CLIAuth.VerificationURL {
		t.Fatalf("cli_verification_url=%q", response.Data.CLIVerificationURL)
	}
	if response.Data.MachinesURL != cfg.CLIAuth.MachinesURL {
		t.Fatalf("machines_url=%q", response.Data.MachinesURL)
	}
}

func TestClientConfigurationPublishesConfiguredDomains(t *testing.T) {
	for _, base := range []string{"example.test", "alternate.test"} {
		t.Run(base, func(t *testing.T) {
			cfg := config.Default()
			cfg.HTTP.PublicBaseURL = "https://api." + base
			cfg.HTTP.BrowserLoginOrigin = "https://console." + base
			cfg.ReleaseBaseURL = "https://get." + base
			cfg.CLIAuth.VerificationURL = "https://console." + base + "/cli/authorize"
			cfg.CLIAuth.MachinesURL = "https://console." + base + "/machines"
			cfg.LocalAccessBaseDomain = "local." + base
			cfg.Preview.BaseDomain = "public." + base
			cfg.Tunnel.BaseDomain = "public." + base
			cfg.RuntimeBaseDomain = "runtime." + base
			w := httptest.NewRecorder()
			clientConfiguration(cfg).ServeHTTP(w, httptest.NewRequest("GET", "/v1/client-configuration", nil))
			var response struct {
				Data map[string]string `json:"data"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
				t.Fatalf("discovery status=%d", w.Code)
			}
			for field, want := range map[string]string{
				"issuer":                   "https://api." + base,
				"installer_base_url":       "https://get." + base,
				"console_url":              "https://console." + base,
				"local_access_base_domain": "local." + base,
				"preview_base_domain":      "public." + base,
				"tunnel_base_domain":       "public." + base,
				"runtime_base_domain":      "runtime." + base,
			} {
				if response.Data[field] != want {
					t.Errorf("%s=%q, want %q", field, response.Data[field], want)
				}
			}
		})
	}
}

func TestClientConfigurationRejectsMissingOrInvalidDeployment(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"missing local domain":    func(c *config.Config) { c.LocalAccessBaseDomain = "" },
		"invalid local domain":    func(c *config.Config) { c.LocalAccessBaseDomain = "https://local.example.test" },
		"missing installer":       func(c *config.Config) { c.ReleaseBaseURL = "" },
		"credential in installer": func(c *config.Config) { c.ReleaseBaseURL = "https://private:secret@get.example.test" },
		"invalid preview":         func(c *config.Config) { c.Preview.BaseDomain = "*.public.example.test" },
	} {
		t.Run(name, func(t *testing.T) {
			c := config.Default()
			c.LocalAccessBaseDomain = "local.example.test"
			c.RuntimeBaseDomain = "runtime.example.test"
			c.ReleaseBaseURL = "https://get.example.test"
			c.HTTP.BrowserLoginOrigin = "https://console.example.test"
			mutate(&c)
			w := httptest.NewRecorder()
			clientConfiguration(c).ServeHTTP(w, httptest.NewRequest("GET", "/v1/client-configuration", nil))
			var response ErrorResponse
			if json.Unmarshal(w.Body.Bytes(), &response) != nil || w.Code != 503 || response.Error.Code != "client_configuration_unavailable" {
				t.Fatalf("status=%d code=%s", w.Code, response.Error.Code)
			}
		})
	}
}
