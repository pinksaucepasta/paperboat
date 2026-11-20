package bbr

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cc "github.com/quic-go/quic-go/internal/congestion"
	"github.com/quic-go/quic-go/internal/monotime"
	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"
)

// PAPERBOAT PATCH (BBR): a deterministic one-flow simulation of a bottleneck, driven through the same
// callbacks the ACK handler uses. It checks what the benchmark matrix measures: throughput under random
// loss, and behaviour behind a token-bucket policer.

type fakeClock struct{ now monotime.Time }

func (c *fakeClock) Now() monotime.Time { return c.now }

type simPacket struct {
	pn       protocol.PacketNumber
	size     protocol.ByteCount
	ackAt    monotime.Time // zero: dropped
	sentAt   monotime.Time
	acked    bool
	declared bool
}

type simLink struct {
	rate       float64 // bytes per second of the bottleneck
	rtt        time.Duration
	lossProb   float64
	queueBytes protocol.ByteCount // drop-tail buffer; 0 = unlimited
	// policer: tokens refill at policeRate and hold at most policeBurst bytes; over-rate packets are dropped.
	policeRate  float64
	policeBurst float64
}

type simResult struct {
	goodput  float64 // delivered bytes per second over the second half
	lossRate float64 // fraction of sent packets that were declared lost
	maxQueue time.Duration
}

func simulate(t *testing.T, sender cc.SendAlgorithmWithDebugInfos, rtt *utils.RTTStats, clock *fakeClock, l simLink, d time.Duration) simResult {
	const size = protocol.ByteCount(1200)
	rng := rand.New(rand.NewSource(1))
	start := clock.now
	var (
		pn, largestAcked        protocol.PacketNumber = -1, -1
		inFlight                protocol.ByteCount
		sent                    []*simPacket
		nextFree                monotime.Time // the bottleneck is busy until then
		tokens                  = l.policeBurst
		lastToken               = start
		deliveredHalf, lostPkts int
		maxQueue                time.Duration
		pendingAck              []*simPacket
	)
	for clock.now.Sub(start) < d {
		now := clock.now
		// send while the congestion window and the pacer allow
		for inFlight < sender.GetCongestionWindow() && sender.HasPacingBudget(now) {
			pn++
			p := &simPacket{pn: pn, size: size, sentAt: now}
			inFlight += size
			sender.OnPacketSent(now, inFlight, pn, size, true)
			sent = append(sent, p)
			drop := rng.Float64() < l.lossProb
			if l.policeRate > 0 {
				tokens = min(l.policeBurst, tokens+l.policeRate*now.Sub(lastToken).Seconds())
				lastToken = now
				if tokens < float64(size) {
					drop = true
				} else {
					tokens -= float64(size)
				}
			}
			begin := max(now, nextFree)
			if queued := begin.Sub(now); l.queueBytes > 0 && float64(queued)*l.rate/1e9 > float64(l.queueBytes) {
				drop = true
			} else if !drop {
				nextFree = begin.Add(time.Duration(float64(size) / l.rate * 1e9))
				maxQueue = max(maxQueue, queued)
				p.ackAt = nextFree.Add(l.rtt)
				pendingAck = append(pendingAck, p)
			}
		}
		// deliver this tick's acknowledgements as one batch, like one ACK frame
		var acked []cc.AckedPacketInfo
		var lost []cc.LostPacketInfo
		prior := inFlight
		var newest *simPacket
		kept := pendingAck[:0]
		for _, p := range pendingAck {
			if p.ackAt.After(now) {
				kept = append(kept, p)
				continue
			}
			p.acked = true
			acked = append(acked, cc.AckedPacketInfo{PacketNumber: p.pn, BytesAcked: p.size})
			if p.pn > largestAcked {
				largestAcked, newest = p.pn, p
			}
			if now.Sub(start) > d/2 {
				deliveredHalf++
			}
		}
		pendingAck = kept
		if len(acked) > 0 {
			rtt.UpdateRTT(now.Sub(newest.sentAt), 0)
			for _, p := range sent {
				if !p.acked && !p.declared && p.pn+3 <= largestAcked { // packet threshold loss detection
					p.declared = true
					lostPkts++
					lost = append(lost, cc.LostPacketInfo{PacketNumber: p.pn, BytesLost: p.size})
				}
			}
			for _, a := range acked {
				inFlight -= a.BytesAcked
			}
			for _, x := range lost {
				inFlight -= x.BytesLost
			}
			if ex, ok := sender.(cc.SendAlgorithmEx); ok {
				ex.OnCongestionEventEx(prior, now, acked, lost)
			} else { // the ACK handler's per-packet order for Reno
				for _, x := range lost {
					sender.OnCongestionEvent(x.PacketNumber, x.BytesLost, prior)
				}
				for _, a := range acked {
					sender.OnPacketAcked(a.PacketNumber, a.BytesAcked, prior, now)
				}
				sender.OnAckFrame(largestAcked, pn, rtt.LatestRTT())
			}
			// forget settled packets
			live := sent[:0]
			for _, p := range sent {
				if !p.acked && !p.declared {
					live = append(live, p)
				}
			}
			sent = live
		}
		clock.now = clock.now.Add(100 * time.Microsecond)
	}
	return simResult{
		goodput:  float64(deliveredHalf) * float64(size) / (d / 2).Seconds(),
		lossRate: float64(lostPkts) / float64(pn+1),
		maxQueue: maxQueue,
	}
}

func newSimSender() (*bbrSender, *utils.RTTStats, *fakeClock) {
	clock := &fakeClock{now: monotime.Now()}
	rtt := utils.NewRTTStats()
	return NewBbrSender(clock, rtt, &utils.ConnectionStats{}, 1200, ProfileStandard), rtt, clock
}

// 20 Mbit/s, 50 ms: with 0.5% random loss BBR keeps most of the link (NewReno's Mathis ceiling is ~3 Mbit/s here).
func TestBBRKeepsThroughputUnderRandomLoss(t *testing.T) {
	sender, rtt, clock := newSimSender()
	link := simLink{rate: 20e6 / 8, rtt: 50 * time.Millisecond, lossProb: 0.005, queueBytes: 150_000}
	r := simulate(t, sender, rtt, clock, link, 20*time.Second)
	t.Logf("goodput %.1f Mbit/s, loss declared %.2f%%, max queue %v", r.goodput*8/1e6, r.lossRate*100, r.maxQueue)
	require.Greater(t, r.goodput*8/1e6, 14.0, "BBR must stay near the link rate under 0.5%% random loss")
	require.NotZero(t, sender.connStats.PacketsLost.Load(), "BBR reports its losses in the connection statistics")
}

// The same link under a Reno sender collapses: this keeps the comparison honest.
func TestRenoCollapsesUnderRandomLoss(t *testing.T) {
	clock := &fakeClock{now: monotime.Now()}
	rtt := utils.NewRTTStats()
	reno := cc.NewCubicSender(clock, rtt, &utils.ConnectionStats{}, 1200, true, nil)
	link := simLink{rate: 20e6 / 8, rtt: 50 * time.Millisecond, lossProb: 0.005, queueBytes: 150_000}
	r := simulate(t, reno, rtt, clock, link, 20*time.Second)
	t.Logf("reno goodput %.1f Mbit/s", r.goodput*8/1e6)
	require.Less(t, r.goodput*8/1e6, 8.0)
}

// A policer (no queue, burst of 20 ms) drops everything over the rate. BBR's windowed maximum must settle near the
// policed rate, with a bounded loss rate rather than a collapse.
func TestBBRBehindPolicer(t *testing.T) {
	sender, rtt, clock := newSimSender()
	rate := 20e6 / 8
	link := simLink{rate: 1e9 / 8, rtt: 50 * time.Millisecond, policeRate: rate, policeBurst: rate * 0.02}
	r := simulate(t, sender, rtt, clock, link, 30*time.Second)
	t.Logf("policed goodput %.1f Mbit/s of 20, loss declared %.2f%%", r.goodput*8/1e6, r.lossRate*100)
	require.Greater(t, r.goodput*8/1e6, 10.0)
	require.Less(t, r.lossRate, 0.35)
}

// An idle sender that restarts reports zero bytes in flight before its first packet.
func TestSendAfterIdleCountsBytesInFlightBeforeThePacket(t *testing.T) {
	sender, _, clock := newSimSender()
	sender.OnPacketSent(clock.now, 1200, 0, 1200, true) // quic-go counts the packet itself
	require.True(t, sender.exitingQuiescence)
	sender.exitingQuiescence = false
	sender.OnPacketSent(clock.now, 2400, 1, 1200, true)
	require.False(t, sender.exitingQuiescence)
	require.Equal(t, protocol.ByteCount(1200), sender.bytesInFlight)
}

// An ACK frame that only acknowledges packets outside the bytes in flight reaches the sender with empty batches.
func TestEmptyCongestionEventIsIgnored(t *testing.T) {
	sender, _, clock := newSimSender()
	require.NotPanics(t, func() { sender.OnCongestionEventEx(0, clock.now, nil, nil) })
}
