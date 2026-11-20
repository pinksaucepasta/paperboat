package congestion

import (
	"time"

	"github.com/quic-go/quic-go/internal/monotime"
	"github.com/quic-go/quic-go/internal/protocol"
)

// PAPERBOAT PATCH (BBR): the sender chosen per connection by quic.Config.CongestionControl.
// Reno (the zero value) and Cubic both use the HyStart++ slow start.
type Algorithm uint8

const (
	AlgorithmReno Algorithm = iota
	AlgorithmCubic
	AlgorithmBBR
)

// A SendAlgorithm performs congestion control
type SendAlgorithm interface {
	TimeUntilSend(bytesInFlight protocol.ByteCount) monotime.Time
	HasPacingBudget(now monotime.Time) bool
	OnPacketSent(sentTime monotime.Time, bytesInFlight protocol.ByteCount, packetNumber protocol.PacketNumber, bytes protocol.ByteCount, isRetransmittable bool)
	CanSend(bytesInFlight protocol.ByteCount) bool
	OnAckFrame(largestAcked, largestSent protocol.PacketNumber, rtt time.Duration) // PAPERBOAT PATCH (RFC 9406)
	OnPacketAcked(number protocol.PacketNumber, ackedBytes protocol.ByteCount, priorInFlight protocol.ByteCount, eventTime monotime.Time)
	OnCongestionEvent(number protocol.PacketNumber, lostBytes protocol.ByteCount, priorInFlight protocol.ByteCount)
	OnRetransmissionTimeout(packetsRetransmitted bool)
	SetMaxDatagramSize(protocol.ByteCount)
}

// PAPERBOAT PATCH (BBR): per-ACK batch of what one ACK frame acknowledged or declared lost.
type AckedPacketInfo struct {
	PacketNumber protocol.PacketNumber
	BytesAcked   protocol.ByteCount
	ReceivedTime monotime.Time
}

type LostPacketInfo struct {
	PacketNumber protocol.PacketNumber
	BytesLost    protocol.ByteCount
}

// A SendAlgorithmEx wants the acked and lost packets of one ACK together (BBR's bandwidth sampler).
// The slices are reused by the caller: implementations must not keep them.
type SendAlgorithmEx interface {
	OnCongestionEventEx(priorInFlight protocol.ByteCount, eventTime monotime.Time, ackedPackets []AckedPacketInfo, lostPackets []LostPacketInfo)
}

// A SendAlgorithmWithDebugInfos is a SendAlgorithm that exposes some debug infos
type SendAlgorithmWithDebugInfos interface {
	SendAlgorithm
	InSlowStart() bool
	InRecovery() bool
	GetCongestionWindow() protocol.ByteCount
}
