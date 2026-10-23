package httpapi

import (
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

type clientConfigurationResponse struct {
	Issuer                string `json:"issuer"`
	InstallerBaseURL      string `json:"installer_base_url"`
	ConsoleURL            string `json:"console_url"`
	LocalAccessBaseDomain string `json:"local_access_base_domain"`
	PreviewBaseDomain     string `json:"preview_base_domain"`
	TunnelBaseDomain      string `json:"tunnel_base_domain"`
	RuntimeBaseDomain     string `json:"runtime_base_domain"`
	Version               string `json:"version"`
	CLIVerificationURL    string `json:"cli_verification_url"`
	MachinesURL           string `json:"machines_url"`
}

func clientConfiguration(cfg config.Config) http.HandlerFunc {
	response := clientConfigurationResponse{
		Version:               "1",
		Issuer:                config.NormalizeIssuer(cfg.HTTP.PublicBaseURL),
		InstallerBaseURL:      cfg.ReleaseBaseURL,
		ConsoleURL:            cfg.HTTP.BrowserLoginOrigin,
		LocalAccessBaseDomain: cfg.LocalAccessBaseDomain,
		PreviewBaseDomain:     cfg.Preview.BaseDomain,
		TunnelBaseDomain:      cfg.Tunnel.BaseDomain,
		RuntimeBaseDomain:     cfg.RuntimeBaseDomain,
		CLIVerificationURL:    cfg.CLIAuth.VerificationURL,
		MachinesURL:           cfg.CLIAuth.MachinesURL,
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
