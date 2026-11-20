// PAPERBOAT PATCH (RFC 9406): verify the round, delay, and CSS rules.
package congestion

import (
	"testing"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/stretchr/testify/require"
)

func TestHyStartPlusPlusDelayThreshold(t *testing.T) {
	for _, tc := range []struct {
		name            string
		base, threshold time.Duration
	}{
		{"minimum", 8 * time.Millisecond, 4 * time.Millisecond},
		{"fraction", 80 * time.Millisecond, 10 * time.Millisecond},
		{"maximum", 500 * time.Millisecond, 16 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h HybridSlowStart
			// Equality ends the round. ACKs without a fresh RTT don't supply samples.
			require.False(t, h.onAck(10, 10, tc.base))
			for n := range protocol.PacketNumber(7) {
				h.onAck(11+n, 40, tc.base+tc.threshold)
			}
			h.onAck(18, 40, 0)
			require.Zero(t, h.cssBaseline)
			h.onAck(19, 40, tc.base+tc.threshold)
			require.Equal(t, tc.base+tc.threshold, h.cssBaseline)

			// The comparator is inclusive, but a sample just below the threshold
			// prevents entry even if all the others are above it.
			h = HybridSlowStart{}
			h.onAck(10, 10, tc.base)
			h.onAck(11, 40, tc.base+tc.threshold-time.Nanosecond)
			for n := range protocol.PacketNumber(8) {
				h.onAck(12+n, 40, tc.base+tc.threshold)
			}
			require.Zero(t, h.cssBaseline)
		})
	}
}

func TestHyStartPlusPlusUsesPreviousRoundMinimum(t *testing.T) {
	var h HybridSlowStart
	h.onAck(5, 5, 50*time.Millisecond)
	h.onAck(10, 10, 80*time.Millisecond)
	// A comparison to the connection minimum would incorrectly enter CSS.
	for pn := protocol.PacketNumber(11); pn <= 18; pn++ {
		h.onAck(pn, 20, 89*time.Millisecond)
	}
	require.Zero(t, h.cssBaseline)
	h.onAck(20, 30, 0)
	for pn := protocol.PacketNumber(21); pn <= 28; pn++ {
		h.onAck(pn, 30, 101*time.Millisecond)
	}
	require.Equal(t, 101*time.Millisecond, h.cssBaseline)
}

func TestHyStartPlusPlusConservativeRounds(t *testing.T) {
	for _, recover := range []bool{false, true} {
		name := "persistent delay"
		if recover {
			name = "transient delay"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestCubicSender(true).sender
			ack := func(pn, sent protocol.PacketNumber, rtt time.Duration, growth protocol.ByteCount) {
				t.Helper()
				before := s.GetCongestionWindow()
				s.OnPacketAcked(pn, 1000, before, 0)
				s.OnAckFrame(pn, sent, rtt)
				require.Equal(t, growth, s.GetCongestionWindow()-before)
			}
			ack(10, 10, 500*time.Millisecond, 1000)
			for pn := protocol.PacketNumber(11); pn <= 18; pn++ {
				ack(pn, 20, 516*time.Millisecond, 1000)
			}
			require.True(t, s.InSlowStart())
			ack(20, 30, 516*time.Millisecond, 250) // completes the partial CSS round
			if recover {
				for pn := protocol.PacketNumber(21); pn < 30; pn++ {
					rtt := 516 * time.Millisecond
					if pn == 29 {
						rtt--
					}
					ack(pn, 30, rtt, 250)
				}
				ack(30, 40, 500*time.Millisecond, 1000)
				require.True(t, s.InSlowStart())
				return
			}
			for _, pn := range []protocol.PacketNumber{30, 40, 50, 60} {
				require.True(t, s.InSlowStart())
				ack(pn, pn+10, 516*time.Millisecond, 250)
			}
			require.False(t, s.InSlowStart())
			require.Equal(t, s.GetCongestionWindow(), s.slowStartThreshold)
		})
	}
}

func TestHyStartPlusPlusCongestionEndsCSS(t *testing.T) {
	for _, lostBytes := range []protocol.ByteCount{0, 1000} { // ECN and loss share the congestion event path.
		s := newTestCubicSender(true).sender
		s.OnAckFrame(10, 10, 500*time.Millisecond)
		for pn := protocol.PacketNumber(11); pn <= 18; pn++ {
			s.OnAckFrame(pn, 20, 516*time.Millisecond)
		}
		require.Positive(t, s.hybridSlowStart.cssBaseline)
		s.OnPacketSent(0, 0, 22, 1000, true)
		before := s.GetCongestionWindow()
		s.OnCongestionEvent(21, lostBytes, before)
		require.False(t, s.InSlowStart())
		require.Less(t, s.GetCongestionWindow(), before)
		s.OnRetransmissionTimeout(true)
		require.True(t, s.InSlowStart())
		for pn := protocol.PacketNumber(23); pn < 33; pn++ {
			s.OnAckFrame(pn, 40, 516*time.Millisecond)
		}
		require.False(t, s.hybridSlowStart.started)
	}
}
