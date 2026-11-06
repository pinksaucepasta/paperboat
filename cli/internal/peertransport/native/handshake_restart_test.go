package native

import (
	"testing"
	"time"
)

// A fixed 300 ms wait can never complete a WireGuard plus QUIC handshake on a relay or long-haul path, so every dial
// after the peer's clean close would fail and pay a fresh handshake. The wait follows the peer's own measured handshake.
func TestRestartHandshakeFollowsThePeersMeasuredHandshake(t *testing.T) {
	for _, tc := range []struct {
		took time.Duration
		want time.Duration
	}{
		{0, 300 * time.Millisecond},                      // nothing measured: the floor
		{47 * time.Millisecond, 300 * time.Millisecond},  // same-host or LAN
		{120 * time.Millisecond, 360 * time.Millisecond}, // regional: three round trips
		{250 * time.Millisecond, 750 * time.Millisecond}, // long haul
		{334 * time.Millisecond, 0},                      // needs a third of the normal second: normal timeout
		{900 * time.Millisecond, 0},
	} {
		if got := restartHandshake(tc.took); got != tc.want {
			t.Errorf("restartHandshake(%v) = %v, want %v", tc.took, got, tc.want)
		}
	}
}
