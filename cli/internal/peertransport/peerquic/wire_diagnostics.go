package peerquic

import (
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

// Keep the existing PTO observer, but correlate each connection's metadata.
// Never encode qlog events wholesale: headers and frames can contain secrets.
type wireTrace struct {
	pto *ptoTrace
	id  string
}

func (*wireTrace) SupportsSchemas(schema string) bool { return schema == qlog.EventSchema }
func (t *wireTrace) AddProducer() qlogwriter.Recorder {
	return &ptoRecorder{ptoTrace: t.pto, connection: t.id}
}

func (r *ptoRecorder) recordWireEvent(event qlogwriter.Event) {
	fields := []any{"trace_id", r.id, "quic_id", r.connection}
	switch e := event.(type) {
	case qlog.StartedConnection:
		if !diagnostics.TerminalHopsEnabled() {
			return
		}
		local, remote := e.Local.IPv6, e.Remote.IPv6
		if !local.IsValid() {
			local = e.Local.IPv4
		}
		if !remote.IsValid() {
			remote = e.Remote.IPv4
		}
		fields = append(fields, "event", "connection_started", "hop_label", diagnostics.ConnectionHopLabel(local.String(), remote.String()))
	case *qlog.StartedConnection:
		r.recordWireEvent(*e)
		return
	case qlog.CongestionStateUpdated:
		if !diagnostics.TerminalHopsEnabled() {
			return
		}
		fields = append(fields, "event", "congestion_state", "state", e.State.String())
	case *qlog.CongestionStateUpdated:
		r.recordWireEvent(*e)
		return
	case qlog.LossTimerUpdated:
		if !diagnostics.TerminalHopsEnabled() {
			return
		}
		fields = append(fields, "event", "loss_timer", "stage", string(e.Type), "timer_type", string(e.TimerType))
		if !e.Time.IsZero() {
			fields = append(fields, "timer_unix_us", e.Time.UnixMicro())
		}
	case *qlog.LossTimerUpdated:
		r.recordWireEvent(*e)
		return
	case qlog.ParametersSet:
		if !diagnostics.TerminalHopsEnabled() {
			return
		}
		fields = append(fields, "event", "flow_parameters", "initiator", string(e.Initiator), "limit", int64(e.InitialMaxData), "stream_local_limit", int64(e.InitialMaxStreamDataBidiLocal), "stream_remote_limit", int64(e.InitialMaxStreamDataBidiRemote))
	case *qlog.ParametersSet:
		r.recordWireEvent(*e)
		return
	case qlog.MetricsUpdated:
		if !diagnostics.TerminalHopsEnabled() {
			return
		}
		fields = append(fields, "event", "metrics", "rtt_us", e.SmoothedRTT.Microseconds(), "rtt_variance_us", e.RTTVariance.Microseconds(), "bytes_in_flight", e.BytesInFlight, "congestion_window", e.CongestionWindow)
	case *qlog.MetricsUpdated:
		r.recordWireEvent(*e)
		return
	case qlog.PacketSent:
		r.recordPacket("sent", e.Header, e.Frames, uint64(e.DatagramPayloadChecksum))
		return
	case *qlog.PacketSent:
		r.recordPacket("sent", e.Header, e.Frames, uint64(e.DatagramPayloadChecksum))
		return
	case qlog.PacketReceived:
		r.recordPacket("received", e.Header, e.Frames, uint64(e.DatagramPayloadChecksum))
		return
	case *qlog.PacketReceived:
		r.recordPacket("received", e.Header, e.Frames, uint64(e.DatagramPayloadChecksum))
		return
	case qlog.PacketDropped:
		fields = append(fields, "event", "dropped", "packet_type", string(e.Header.PacketType), "trigger", string(e.Trigger))
	case *qlog.PacketDropped:
		r.recordWireEvent(*e)
		return
	case qlog.PacketLost:
		fields = append(fields, "event", "lost", "packet_type", string(e.Header.PacketType), "packet_number", int64(e.Header.PacketNumber), "trigger", string(e.Trigger))
	case *qlog.PacketLost:
		r.recordWireEvent(*e)
		return
	case qlog.PTOCountUpdated:
		fields = append(fields, "event", "pto", "pto_count", uint64(e.PTOCount))
	case *qlog.PTOCountUpdated:
		r.recordWireEvent(*e)
		return
	case qlog.ConnectionClosed:
		fields = append(fields, "event", "closed", "initiator", string(e.Initiator), "trigger", string(e.Trigger), "application_close", e.ApplicationError != nil)
	case *qlog.ConnectionClosed:
		r.recordWireEvent(*e)
		return
	default:
		return
	}
	diagnosticlog.TryInfo("peer QUIC wire", fields...)
}

func (r *ptoRecorder) recordPacket(direction string, header qlog.PacketHeader, frames []qlog.Frame, checksum uint64) {
	if diagnostics.TerminalHopsEnabled() {
		r.recordStreamFrames(direction, header, frames, checksum)
	}
	closeFrame := false
	for _, frame := range frames {
		if _, ok := frame.Frame.(*qlog.ConnectionCloseFrame); ok {
			closeFrame = true
		}
	}
	// Application packet contents and per-keystroke traffic are never recorded.
	if !closeFrame && header.PacketType != qlog.PacketTypeInitial && header.PacketType != qlog.PacketTypeHandshake {
		return
	}
	diagnosticlog.TryInfo("peer QUIC wire", "trace_id", r.id, "quic_id", r.connection, "event", direction,
		"packet_type", string(header.PacketType), "packet_number", int64(header.PacketNumber), "close_frame", closeFrame)
}

var _ qlogwriter.Trace = (*wireTrace)(nil)

// STREAM ranges identify retransmission of the same bytes without retaining application data.
func (r *ptoRecorder) recordStreamFrames(direction string, header qlog.PacketHeader, frames []qlog.Frame, checksum uint64) {
	for _, frame := range frames {
		fields := []any{"trace_id", r.id, "quic_id", r.connection, "event", direction, "packet_number", int64(header.PacketNumber), "datagram_checksum", checksum}
		switch f := frame.Frame.(type) {
		case *qlog.StreamFrame:
			fields = append(fields, "stage", "stream_frame", "stream_id", int64(f.StreamID), "stream_offset", f.Offset, "stream_length", f.Length)
		case *qlog.MaxDataFrame:
			fields = append(fields, "stage", "connection_flow_limit", "limit", int64(f.MaximumData))
		case *qlog.MaxStreamDataFrame:
			fields = append(fields, "stage", "stream_flow_limit", "stream_id", int64(f.StreamID), "limit", int64(f.MaximumStreamData))
		case *qlog.DataBlockedFrame:
			fields = append(fields, "stage", "connection_flow_blocked", "limit", int64(f.MaximumData))
		case *qlog.StreamDataBlockedFrame:
			fields = append(fields, "stage", "stream_flow_blocked", "stream_id", int64(f.StreamID), "limit", int64(f.MaximumStreamData))
		default:
			continue
		}
		diagnosticlog.TryInfo("peer QUIC wire", fields...)
	}
}
