package app

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Global nodes are named beside the API host, never under a domain users' names live in.
func TestInfrastructureDomainIsTheAPIHostsDomain(t *testing.T) {
	for url, want := range map[string]string{
		"https://api.infra.test": "infra.test", "https://API.infra.test:8443": "infra.test", "https://infra.test": "",
		"https://127.0.0.1": "", "https://localhost:1": "", "https://api.previews.test": "", "https://api.runtime.test": "",
	} {
		cfg := config.Config{RuntimeBaseDomain: "runtime.test"}
		cfg.HTTP.PublicBaseURL, cfg.Preview.BaseDomain = url, "previews.test"
		if got := infrastructureDomain(cfg); got != want {
			t.Errorf("%s: %q, want %q", url, got, want)
		}
		cfg.Certificates.DNSZoneID = "zone"
		if newNodeNames(nil, cfg, nil) != nil {
			t.Errorf("%s: named without ManageEdgeWildcards", url)
		}
		cfg.Certificates.ManageEdgeWildcards = true
		if names := newNodeNames(nil, cfg, nil); (names != nil) != (want != "") {
			t.Errorf("%s: names %v", url, names)
		}
	}
}
