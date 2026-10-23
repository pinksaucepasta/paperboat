//go:build darwin || linux

package hostruntimecmd

import "testing"

func TestInstallControlURLPrefersTheSelectedServer(t *testing.T) {
	if got := installControlURL("https://api.staging.example.test:8443", "https://api.example.test"); got != "https://api.staging.example.test:8443" {
		t.Fatalf("selected: %q", got)
	}
	if got := installControlURL("", "https://api.example.test"); got != "https://api.example.test" {
		t.Fatalf("configured: %q", got)
	}
}
