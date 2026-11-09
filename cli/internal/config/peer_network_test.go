package config

import (
	"strings"
	"testing"
)

// The kept configuration is what an offline restart restores (F21-01): it comes back byte for byte, at a size a real
// fleet reaches, and saving "" clears it. A Windows credential file would cap it at 5 KiB and return an encrypted blob.
func TestPeerNetworkConfigurationIsKeptVerbatimAtFleetSize(t *testing.T) {
	store := ProfileStore{Path: t.TempDir()}
	token := strings.Repeat("header.body.signature-", 4096) // about 90 KiB
	const issuer, account, endpoint = "https://api.example.test", "account_test", "cli_test"
	if err := store.SavePeerNetworkConfiguration(issuer, account, endpoint, token); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadPeerNetworkConfiguration(issuer, account, endpoint, len(token))
	if err != nil || got != token {
		t.Fatalf("kept configuration: %d bytes, %v; want the %d bytes saved", len(got), err, len(token))
	}
	if _, err := store.LoadPeerNetworkConfiguration(issuer, account, endpoint, len(token)-1); err == nil {
		t.Fatal("a configuration over the limit was returned")
	}
	if err := store.SavePeerNetworkConfiguration(issuer, account, endpoint, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := store.LoadPeerNetworkConfiguration(issuer, account, endpoint, len(token)); err != nil || got != "" {
		t.Fatalf("cleared configuration = %d bytes, %v", len(got), err)
	}
}
