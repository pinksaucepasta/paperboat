//go:build linux || darwin || windows

package hostinstall

import (
	"testing"
	"time"
)

// A first carrier's service start includes Defender's first scan of the 99 MB executable on a laptop: it got 15 s
// and failed a first pair on victus (31 s in). Keep a margin over that.
func TestCarrierGetsLongerThanAFirstExecutableScan(t *testing.T) {
	if carrierReadyTimeout < 45*time.Second {
		t.Fatalf("carrier readiness window %v is shorter than a measured first-start scan", carrierReadyTimeout)
	}
}
