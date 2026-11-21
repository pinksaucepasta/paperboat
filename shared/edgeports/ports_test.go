package edgeports

import "testing"

func TestRangesAreValidAndDisjoint(t *testing.T) {
	if err := Check(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicTCPBounds(t *testing.T) {
	for port, want := range map[int]bool{PublicTCPMin - 1: false, PublicTCPMin: true, 24443: true, PublicTCPMax: true, PublicTCPMax + 1: false, 443: false} {
		if PublicTCP(port) != want {
			t.Errorf("PublicTCP(%d) = %v, want %v", port, !want, want)
		}
	}
}

func TestNodePortsAreDistinctAndClearOfThePublicRange(t *testing.T) {
	seen := map[int]bool{}
	for _, p := range []int{CarrierTCP, CarrierQUIC, RelayWSS, RelayQUIC, RelayMesh, Claim, EdgeHealth, EdgePrivateHTTPS} {
		if p < 1024 || p >= PublicTCPMin || seen[p] {
			t.Fatalf("node port %d is privileged, inside the public range or repeated", p)
		}
		seen[p] = true
	}
}
