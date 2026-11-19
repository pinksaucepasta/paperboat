package machineguard

import (
	"bytes"
	"os"
	"testing"
)

// The browser-domain selection 2026.11.16.2 wrote decodes after an update.
func TestDecodesBrowserDomainsWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/2026.11.16.2-browser-domains.json")
	if err != nil {
		t.Fatal(err)
	}
	// The previous release keyed owners by uid on Unix and by SID on Windows; the checked-in fixture carries the Unix form.
	owner := "1000"
	if !validBrowserDomainOwner(owner) {
		owner = "S-1-5-21-1-2-3-1000"
	}
	raw = bytes.Replace(raw, []byte(`"1000"`), []byte(`"`+owner+`"`), 1)
	domains, err := decodeBrowserDomains(raw)
	if err != nil || domains.Owners[owner] != testBrowserDomain {
		t.Fatalf("previous release's browser domains = %+v, %v", domains, err)
	}
}
