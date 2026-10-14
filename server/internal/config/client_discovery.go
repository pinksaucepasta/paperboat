package config

import (
	"fmt"
	"net"
	"net/url"
)

// ValidateClientDiscovery rejects incomplete discovery instead of publishing
// defaults that clients could mistake for deployment-owned authority.
func (c Config) ValidateClientDiscovery() error {
	for name, value := range map[string]string{
		"local_access_base_domain": c.LocalAccessBaseDomain,
		"preview.base_domain":      c.Preview.BaseDomain,
		"tunnel.base_domain":       c.Tunnel.BaseDomain,
		"runtime_base_domain":      c.RuntimeBaseDomain,
	} {
		if !validEndpointBaseDomain(value) || net.ParseIP(value) != nil {
			return fmt.Errorf("client discovery requires configured %s", name)
		}
	}
	for name, value := range map[string]string{
		"http.public_base_url":      c.HTTP.PublicBaseURL,
		"http.browser_login_origin": c.HTTP.BrowserLoginOrigin,
		"release_base_url":          c.ReleaseBaseURL,
		"cli_auth.verification_url": c.CLIAuth.VerificationURL,
		"cli_auth.machines_url":     c.CLIAuth.MachinesURL,
	} {
		u, err := url.Parse(value)
		if err != nil || len(value) > 2048 || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(c.Environment != EnvironmentProduction && u.Scheme == "http")) {
			return fmt.Errorf("client discovery requires a valid configured %s", name)
		}
	}
	return nil
}
