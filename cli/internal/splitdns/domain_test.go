package splitdns

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBrowserDomainRejectsBroadAndInvalidTrust(t *testing.T) {
	for _, domain := range []string{"com", "co.uk", "github.io", "localhost", "*.mynet.xyz", "https://mynet.xyz", "127.0.0.1", "mynet.xyz:443", "mynet.xyz.", "my_net.xyz", "éxample.com"} {
		if _, err := NormalizeBrowserDomain(domain); err == nil {
			t.Fatalf("unsafe domain accepted: %s", domain)
		}
	}
	for _, domain := range []string{BrowserSuffix, "mynet.xyz", "private.example.co.uk"} {
		if got, err := NormalizeBrowserDomain(domain); err != nil || got != domain {
			t.Fatalf("valid domain %s rejected: %q %v", domain, got, err)
		}
	}
}
func TestParseBrowserHostname(t *testing.T) {
	for _, tc := range []struct{ host, machine, label string }{{"hp.mynet.xyz", "hp", ""}, {"3000.hp.mynet.xyz", "hp", "3000"}, {"jellyfin.hp.mynet.xyz", "hp", "jellyfin"}} {
		m, l, err := ParseBrowserHostname(tc.host, "mynet.xyz")
		if err != nil || m != tc.machine || l != tc.label {
			t.Fatalf("%s: %s %s %v", tc.host, m, l, err)
		}
	}
	for _, host := range []string{"mynet.xyz", "HP.mynet.xyz", "x.y.hp.mynet.xyz", "0001.hp.mynet.xyz", "65536.hp.mynet.xyz", "hp.mynet.xyz.other", "*.hp.mynet.xyz"} {
		if _, _, err := ParseBrowserHostname(host, "mynet.xyz"); err == nil {
			t.Fatalf("invalid hostname accepted: %s", host)
		}
	}
}

func TestRuntimeConstrainedCALoadDoesNotCreateState(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing")
	if _, err := LoadConstrainedCA(directory, "mynet.xyz"); err == nil {
		t.Fatal("missing provisioned root accepted")
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("runtime recreated retired CA directory: %v", err)
	}
	ca, err := LoadOrCreateConstrainedCA(directory, "mynet.xyz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConstrainedCA(directory, "other.xyz"); err == nil {
		t.Fatal("foreign namespace root accepted")
	}
	loaded, err := LoadConstrainedCA(directory, "mynet.xyz")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca.CertPEM(), loaded.CertPEM()) {
		t.Fatal("restart replaced provisioned root")
	}
	if _, _, err := loaded.IssueCertificate([]string{"*.other.xyz"}); err == nil {
		t.Fatal("root signed another namespace")
	}
}
