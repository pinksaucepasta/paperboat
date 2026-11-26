package mesh

import (
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// An engine without a relay region, and the same engine when its first region arrives, never rebinds its socket:
// with no region netcheck sends no probe, which once read as a send error, and each rebind reset every peer path
// (every endpoint update while relay-less, and once more on the first region, around the first open).
func TestNoRebindWithoutRelayOrWhenTheFirstRegionArrives(t *testing.T) {
	var rebinds atomic.Int32
	s := &Server{Key: key.NewNode(), LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::40"), AllowedPeers: map[key.NodePublic]netip.Addr{},
		Logf: func(format string, args ...any) {
			if line := fmt.Sprintf(format, args...); strings.Contains(line, "Rebinding") {
				rebinds.Add(1)
				t.Log(line)
			}
		}}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for len(s.LocalEndpoints()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if len(s.LocalEndpoints()) == 0 {
		t.Fatal("the engine found no local endpoint")
	}
	region := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1").Regions[1]
	if err := s.SetRelayRegions([]*tailcfg.DERPRegion{region}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second) // the ReSTUN a new map triggers, and its endpoint update
	if n := rebinds.Load(); n != 0 {
		t.Fatalf("%d rebinds, want none", n)
	}
}
