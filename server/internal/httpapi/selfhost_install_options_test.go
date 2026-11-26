package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

func TestSelfhostInstallOptionsComeFromServerConfig(t *testing.T) {
	cfg := config.Default()
	cfg.LocalAccessBaseDomain = "local.example.test"
	cfg.RuntimeBaseDomain = "runtime.example.test"
	cfg.Preview.BaseDomain = "preview.example.test"
	cfg.Tunnel.BaseDomain = "tunnel.example.test"
	cfg.ReleaseBaseURL = "https://get.example.test/"
	cfg.HTTP.BrowserLoginOrigin = "https://console.example.test"
	cfg.CLIAuth.VerificationURL = "https://console.example.test/cli/authorize"
	cfg.CLIAuth.MachinesURL = "https://console.example.test/machines/add"
	w := httptest.NewRecorder()
	selfhostInstallOptionsGet(cfg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/selfhost/install-options", nil))
	want := `{"data":{"installer_url":"https://get.example.test/selfhost",` +
		`"server_flags":["--preview-domain","preview.example.test","--tunnel-domain","tunnel.example.test","--runtime-domain","runtime.example.test","--browser-access"],` +
		`"claim_port":7277,"inbound":{` +
		`"both":[{"protocol":"tcp","ports":"7277"},{"protocol":"tcp","ports":"7274"},{"protocol":"udp","ports":"7274"},{"protocol":"udp","ports":"7275"},{"protocol":"tcp","ports":"443"},{"protocol":"udp","ports":"443"},{"protocol":"tcp","ports":"7272"},{"protocol":"udp","ports":"7273"},{"protocol":"tcp","ports":"10000-59999"}],` +
		`"edge":[{"protocol":"tcp","ports":"7277"},{"protocol":"tcp","ports":"443"},{"protocol":"udp","ports":"443"},{"protocol":"tcp","ports":"7272"},{"protocol":"udp","ports":"7273"},{"protocol":"tcp","ports":"10000-59999"}],` +
		`"relay":[{"protocol":"tcp","ports":"7277"},{"protocol":"tcp","ports":"443"},{"protocol":"udp","ports":"443"},{"protocol":"udp","ports":"7275"}]}}}` + "\n"
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	cfg.ReleaseBaseURL = ""
	w = httptest.NewRecorder()
	selfhostInstallOptionsGet(cfg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/selfhost/install-options", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured release URL served %d", w.Code)
	}
}
