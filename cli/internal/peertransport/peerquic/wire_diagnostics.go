package peerquic

import (
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
	case qlog.PacketSent:
		r.recordPacket("sent", e.Header, e.Frames)
		return
	case *qlog.PacketSent:
		r.recordPacket("sent", e.Header, e.Frames)
		return
	case qlog.PacketReceived:
		r.recordPacket("received", e.Header, e.Frames)
		return
	case *qlog.PacketReceived:
		r.recordPacket("received", e.Header, e.Frames)
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

func (r *ptoRecorder) recordPacket(direction string, header qlog.PacketHeader, frames []qlog.Frame) {
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
