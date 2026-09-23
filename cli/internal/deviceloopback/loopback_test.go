package deviceloopback

import (
	"net/netip"
	"testing"
)

func TestNormalizeCIDR(t *testing.T) {
	for _, value := range []string{"127.1.0.0/16", "127.100.0.0/16", "127.254.0.0/16"} {
		if got, err := NormalizeCIDR(value); err != nil || got != value {
			t.Errorf("NormalizeCIDR(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"127.0.0.0/16", "127.255.0.0/16", "127.101.1.0/16", "127.101.0.0/24", "10.1.0.0/16"} {
		if _, err := NormalizeCIDR(value); err == nil {
			t.Errorf("NormalizeCIDR(%q) succeeded", value)
		}
	}
}

func TestMapCanonicalPreservesHostBits(t *testing.T) {
	got, err := MapCanonical(netip.MustParseAddr("127.100.23.45"), "127.212.0.0/16")
	if err != nil || got.String() != "127.212.23.45" {
		t.Fatalf("MapCanonical = %v, %v", got, err)
	}
	if _, err = MapCanonical(netip.MustParseAddr("127.101.23.45"), DefaultCIDR); err == nil {
		t.Fatal("noncanonical assignment accepted")
	}
}
