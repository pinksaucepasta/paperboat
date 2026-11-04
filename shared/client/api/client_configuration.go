package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// ClientConfiguration contains server-owned URLs used by Paperboat clients.
type ClientConfiguration struct {
	LocalAccessBaseDomain string `json:"local_access_base_domain"`
	Version               string `json:"version"`
	InstallerBaseURL      string `json:"installer_base_url"`
	CLIVerificationURL    string `json:"cli_verification_url"`
	MachinesURL           string `json:"machines_url"`
	ConsoleURL            string `json:"console_url"`
}

func FetchClientConfiguration(ctx context.Context, do func(context.Context, string, string, any, any) error) (ClientConfiguration, error) {
	var out ClientConfiguration
	if err := do(ctx, http.MethodGet, "/v1/client-configuration", nil, &out); err != nil {
		return ClientConfiguration{}, err
	}
	if out.Version != "1" {
		return ClientConfiguration{}, errors.New("paperboat-server returned an unsupported client configuration version")
	}
	machinesURL, err := url.Parse(out.MachinesURL)
	if err != nil || (machinesURL.Scheme != "http" && machinesURL.Scheme != "https") || machinesURL.Host == "" {
		return ClientConfiguration{}, errors.New("paperboat-server returned an invalid machines URL")
	}
	return out, nil
}
