package machineguard

import (
	"os"
	"testing"
)

// The browser-domain selection 2026.11.16.2 wrote decodes after an update.
func TestDecodesBrowserDomainsWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/2026.11.16.2-browser-domains.json")
	if err != nil {
		t.Fatal(err)
	}
	domains, err := decodeBrowserDomains(raw)
	if err != nil || domains.Owners["1000"] != testBrowserDomain {
		t.Fatalf("previous release's browser domains = %+v, %v", domains, err)
	}
}
