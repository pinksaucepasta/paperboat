package main

import (
	"log/slog"
	"runtime"
	"strconv"
	"strings"
)

// Only these authored transport sites may retain primitive metadata. All other
// library logs retain source evidence alone, without evaluating arbitrary attrs.
func retainReconnectMetadata(record slog.Record, fields map[string]string) bool {
	if record.PC == 0 {
		return false
	}
	frame, _ := runtime.CallersFrames([]uintptr{record.PC}).Next()
	owners := map[string]string{
		"terminal output hop":     "/internal/diagnostics.",
		"update preparation step": "/internal/diagnostics.",
		"terminal runtime sample": "/internal/diagnostics.",
		"native underlay packet":  "/internal/peertransport/mesh.",
		"native underlay session": "/internal/peertransport/mesh.",
		"carrier cutover":         "/internal/hostruntime/hostinstall.",
		"windows activation step": "/internal/hostruntime/hostinstall.",
		"windows install phase":   "/internal/hostruntime/hostinstall.",
		"peer QUIC wire":          "/internal/peertransport/peerquic.",
		"peer QUIC close":         "/internal/peertransport/peerquic.",
		"native owner close":      "/internal/peertransport/native.",
		"native handshake path":   "/internal/peertransport/native.",
		"native cache admission":  "/internal/tunnel.",
		"native cache reuse":      "/internal/tunnel.",
		"native packet lifecycle": "/internal/peertransport/tailnet.",
		"native scope admission":  "/internal/peertransport/tailnet.",
		"carrier shutdown":        "/internal/hostruntime/runtime.",
		"carrier control call":    "/internal/hostruntime/carriercontrol.",
		"hostd maintenance busy":  "/internal/hostruntime/hostdproto.",
		"preview owner lease":     "/internal/hostruntime/preview.",
		// Bounded reason codes of the handover admission path (pbvm4 feat8).
		"native worker admission refused":   "/internal/hostruntime/server.",
		"native dispatch admission refused": "/internal/hostruntime/server.",
		"native owner admission refused":    "/internal/hostruntime/workloadbridge.",
		"data carrier closed":               "/internal/hostruntime/connector.",
	}
	owner := owners[record.Message]
	if owner == "" || !strings.Contains(frame.Function, owner) {
		return false
	}
	keys := map[string]bool{
		"hop_source_file": true, "hop_source_line": true, "hop_label": true, "range_start": true, "range_end": true, "observed_unix_us": true,
		"datagram_checksum": true, "timer_type": true, "timer_unix_us": true, "stream_local_limit": true, "stream_remote_limit": true, "stream_id": true, "stream_offset": true, "stream_length": true, "rtt_us": true,
		"rtt_variance_us": true, "bytes_in_flight": true, "congestion_window": true,
		"limit": true, "gc_count": true, "gc_pause_us": true, "sched_pause_us": true,
		"dropped": true, "engine_label": true, "peer_label": true,
		"direction": true, "bytes": true, "packets": true, "endpoint_label": true, "state": true,
		"trace_id": true, "quic_id": true, "event": true, "packet_type": true,
		"packet_number": true, "close_frame": true, "pto_count": true, "initiator": true,
		"trigger": true, "application_close": true, "stage": true, "failed": true,
		"sessions": true, "path_category": true, "predicate_present": true, "allowed": true,
		"resource_present": true, "reason": true, "error_type": true, "capability": true,
		"peer_found": true, "resource_found": true, "capability_found": true, "direction_found": true,
		"duration_ms": true,
	}
	record.Attrs(func(attr slog.Attr) bool {
		if !keys[attr.Key] {
			return true
		}
		var value string
		switch attr.Value.Kind() {
		case slog.KindString:
			value = attr.Value.String()
		case slog.KindBool:
			value = strconv.FormatBool(attr.Value.Bool())
		case slog.KindInt64:
			value = strconv.FormatInt(attr.Value.Int64(), 10)
		case slog.KindUint64:
			value = strconv.FormatUint(attr.Value.Uint64(), 10)
		default:
			return true
		}
		if value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-", r))
		}) == -1 {
			fields[attr.Key] = value
		}
		return true
	})
	if fields["observed_unix_us"] == "" {
		fields["observed_unix_us"] = strconv.FormatInt(record.Time.UnixMicro(), 10)
	}
	return true
}
