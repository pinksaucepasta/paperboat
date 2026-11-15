//go:build linux || darwin || windows

package machineguard

import (
	"encoding/json"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

func TestProtectedBrowserDomainSelectionValidation(t *testing.T) {
	owner := "1000"
	if !validBrowserDomainOwner(owner) {
		owner = "S-1-5-21-1-2-3-1000"
	}
	for _, domain := range []string{"com", "co.uk", "github.io", "*.mynet.xyz", "Mynet.xyz"} {
		data, _ := json.Marshal(browserDomains{Schema: browserDomainSchema, Owners: map[string]string{owner: domain}})
		if _, err := decodeBrowserDomains(data); err == nil {
			t.Fatalf("accepted unsafe protected domain %q", domain)
		}
	}
	shadowed, _ := json.Marshal(browserDomains{Schema: browserDomainSchema, Default: testBrowserDomain, Owners: map[string]string{owner: testBrowserDomain}})
	if _, err := decodeBrowserDomains(shadowed); err == nil {
		t.Fatal("accepted an owner selection equal to the deployment default")
	}
	for _, data := range []string{`{}`, `{"schema":"other","owners":{}}`, `{"schema":"paperboat.browser-domains/v1","owners":{"foreign owner":"mynet.xyz"}}`, `{"schema":"paperboat.browser-domains/v1","owners":null}`} {
		if _, err := decodeBrowserDomains([]byte(data)); err == nil {
			t.Fatalf("accepted malformed selection %s", data)
		}
	}
	data, _ := json.Marshal(browserDomains{Schema: browserDomainSchema, Owners: map[string]string{owner: "mynet.xyz"}})
	if got, err := decodeBrowserDomains(data); err != nil || got.Owners[owner] != "mynet.xyz" {
		t.Fatalf("valid selection: %+v %v", got, err)
	}
}
func TestNamedBrowserAliasesRequireExactNumericTarget(t *testing.T) {
	domain := "mynet.xyz"
	numeric := "8989.hp." + domain
	named := "jellyfin.hp." + domain
	aliases := map[string]string{numeric: splitdns.BrowserGatewayHostname, named: numeric}
	if got := browserAliasTarget(aliases, named, domain); got != numeric {
		t.Fatal("named route did not resolve its numeric registration")
	}
	for _, target := range []string{splitdns.BrowserGatewayHostname, "3000.hp." + domain, "8989.other." + domain, "8989.hp.local.example.test", named, "second.hp." + domain} {
		aliases[named] = target
		aliases["second.hp."+domain] = numeric
		if browserAliasTarget(aliases, named, domain) != "" {
			t.Fatalf("accepted invalid target %q", target)
		}
	}
	aliases[named] = numeric
	delete(aliases, numeric)
	if browserAliasTarget(aliases, named, domain) != "" {
		t.Fatal("named route survived numeric withdrawal")
	}
}

// A top-level name (<name>.<domain>) targets one exact numeric registration of any machine, never a chain.
func TestTopLevelBrowserNameRequiresExactNumericTarget(t *testing.T) {
	domain := "mynet.xyz"
	numeric := "8096.media." + domain
	top := "jellyfin." + domain
	aliases := map[string]string{numeric: splitdns.BrowserGatewayHostname, top: numeric}
	if got := browserAliasTarget(aliases, top, domain); got != numeric {
		t.Fatalf("top-level name resolved to %q", got)
	}
	for _, target := range []string{splitdns.BrowserGatewayHostname, "app.media." + domain, "3000.media." + domain, "gateway." + domain, ""} {
		aliases[top] = target
		aliases["app.media."+domain] = numeric
		if browserAliasTarget(aliases, top, domain) != "" {
			t.Fatalf("accepted invalid top-level target %q", target)
		}
	}
	aliases = map[string]string{numeric: splitdns.BrowserGatewayHostname, "gateway." + domain: numeric}
	if browserAliasTarget(aliases, "gateway."+domain, domain) != "" {
		t.Fatal("accepted the reserved gateway label as a top-level name")
	}
}
