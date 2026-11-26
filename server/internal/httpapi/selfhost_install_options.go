package httpapi

import (
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

type selfhostInstallOptions struct {
	InstallerURL string                      `json:"installer_url"`
	ServerFlags  []string                    `json:"server_flags"`
	ClaimPort    int                         `json:"claim_port"`
	Inbound      map[string][]edgeports.Rule `json:"inbound"`
}

// selfhostInstallOptions is what the admin panel needs to show a Paperboat-run node's install command and
// firewall ports: everything from server config and edgeports, nothing typed in the panel.
func selfhostInstallOptionsGet(cfg config.Config) http.HandlerFunc {
	response := selfhostInstallOptions{
		InstallerURL: strings.TrimRight(cfg.ReleaseBaseURL, "/") + "/selfhost",
		ServerFlags: []string{"--preview-domain", cfg.Preview.BaseDomain, "--tunnel-domain", cfg.Tunnel.BaseDomain,
			"--runtime-domain", cfg.RuntimeBaseDomain, "--browser-access"},
		ClaimPort: edgeports.Claim,
		Inbound: map[string][]edgeports.Rule{
			"relay": edgeports.InboundRules("relay", edgeports.Claim),
			"edge":  edgeports.InboundRules("tunnel", edgeports.Claim),
			"both":  edgeports.InboundRules("both", edgeports.Claim),
		},
	}
	configurationErr := cfg.ValidateClientDiscovery()
	return func(w http.ResponseWriter, r *http.Request) {
		if configurationErr != nil {
			writeError(w, r, http.StatusServiceUnavailable, "client_configuration_unavailable", "Server discovery is not configured; contact the server operator.")
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: response})
	}
}
