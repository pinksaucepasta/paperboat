// PAPERBOAT PATCH (RFC 9406): HyStart++ for the initial paced slow start.
package congestion

import (
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
)

const (
	minRTTThreshold  = 4 * time.Millisecond
	maxRTTThreshold  = 16 * time.Millisecond
	minRTTDivisor    = 8
	minRTTSamples    = 8
	cssGrowthDivisor = 4
	cssRounds        = 5
)

type HybridSlowStart struct {
	disabled           bool
	started            bool
	roundEnd           protocol.PacketNumber
	lastRoundMinRTT    time.Duration
	currentRoundMinRTT time.Duration
	rttSamples         int
	cssBaseline        time.Duration
	completedCSSRounds int
}

// onAck accounts for one ACK frame after its congestion window growth. A zero
// RTT means this frame advanced packet acknowledgments without a fresh sample.
func (s *HybridSlowStart) onAck(largestAcked, largestSent protocol.PacketNumber, rtt time.Duration) (exit bool) {
	if !s.started {
		s.started = true
		s.roundEnd = largestSent
	}
	if rtt > 0 {
		if s.currentRoundMinRTT == 0 || rtt < s.currentRoundMinRTT {
			s.currentRoundMinRTT = rtt
		}
		s.rttSamples++
		if s.rttSamples >= minRTTSamples {
			if s.cssBaseline > 0 {
				if s.currentRoundMinRTT < s.cssBaseline {
					s.cssBaseline = 0
					s.completedCSSRounds = 0
				}
			} else if s.lastRoundMinRTT > 0 {
				threshold := max(minRTTThreshold, min(maxRTTThreshold, s.lastRoundMinRTT/minRTTDivisor))
				if s.currentRoundMinRTT >= s.lastRoundMinRTT+threshold {
					s.cssBaseline = s.currentRoundMinRTT
				}
			}
		}
	}
	if largestAcked < s.roundEnd {
		return false
	}

	s.roundEnd = largestSent
	// With no outstanding flight, let its first ACK establish the next boundary.
	s.started = largestSent > largestAcked
	s.lastRoundMinRTT = s.currentRoundMinRTT
	s.currentRoundMinRTT = 0
	s.rttSamples = 0
	if s.cssBaseline > 0 {
		// The round in which CSS starts counts, even if only part of it used CSS.
		s.completedCSSRounds++
		return s.completedCSSRounds == cssRounds
	}
	return false
}
