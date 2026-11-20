package congestion

import (
	"testing"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"

	"github.com/stretchr/testify/require"
)

func TestHybridSlowStartSimpleCase(t *testing.T) {
	slowStart := HybridSlowStart{}

	packetNumber := protocol.PacketNumber(1)
	endPacketNumber := protocol.PacketNumber(3)
	slowStart.StartReceiveRound(endPacketNumber)

	packetNumber++
	require.False(t, slowStart.IsEndOfRound(packetNumber))

	// Test duplicates.
	require.False(t, slowStart.IsEndOfRound(packetNumber))

	packetNumber++
	require.False(t, slowStart.IsEndOfRound(packetNumber))
	packetNumber++
	require.True(t, slowStart.IsEndOfRound(packetNumber))

	// Test without a new registered end_packet_number;
	packetNumber++
	require.True(t, slowStart.IsEndOfRound(packetNumber))

	endPacketNumber = 20
	slowStart.StartReceiveRound(endPacketNumber)
	for packetNumber < endPacketNumber {
		packetNumber++
		require.False(t, slowStart.IsEndOfRound(packetNumber))
	}
	packetNumber++
	require.True(t, slowStart.IsEndOfRound(packetNumber))
}

func TestHybridSlowStartWithDelay(t *testing.T) {
	slowStart := HybridSlowStart{}
	const rtt = 60 * time.Millisecond
	// We expect to detect the increase at +1/8 of the RTT; hence at a typical
	// RTT of 60ms the detection will happen at 67.5 ms.
	const hybridStartMinSamples = 8 // Number of acks required to trigger.

	endPacketNumber := protocol.PacketNumber(1)
	endPacketNumber++
	slowStart.StartReceiveRound(endPacketNumber)

	// Will not trigger since our lowest RTT in our burst is the same as the long
	// term RTT provided.
	for n := range hybridStartMinSamples {
		require.False(t, slowStart.ShouldExitSlowStart(rtt+time.Duration(n)*time.Millisecond, rtt, 100))
	}
	endPacketNumber++
	slowStart.StartReceiveRound(endPacketNumber)
	for n := 1; n < hybridStartMinSamples; n++ {
		require.False(t, slowStart.ShouldExitSlowStart(rtt+(time.Duration(n)+10)*time.Millisecond, rtt, 100))
	}
	// Expect to trigger since all packets in this burst was above the long term
	// RTT provided.
	require.True(t, slowStart.ShouldExitSlowStart(rtt+10*time.Millisecond, rtt, 100))
}

// PAPERBOAT PATCH: a long path does not leave slow start for jitter below 1/8 of its RTT (upstream cut it at 16 ms,
// which ended slow start on a 500 ms path after 30 ms of jitter).
func TestHybridSlowStartLongPathIgnoresJitterBelowAnEighth(t *testing.T) {
	const minRTT = 500 * time.Millisecond
	for _, c := range []struct {
		extra time.Duration
		exit  bool
	}{{30 * time.Millisecond, false}, {60 * time.Millisecond, false}, {70 * time.Millisecond, true}} {
		slowStart := HybridSlowStart{}
		slowStart.StartReceiveRound(2)
		var exit bool
		for range 8 {
			exit = slowStart.ShouldExitSlowStart(minRTT+c.extra, minRTT, 100)
		}
		require.Equal(t, c.exit, exit, "%v over the minimum RTT", c.extra)
	}
}
