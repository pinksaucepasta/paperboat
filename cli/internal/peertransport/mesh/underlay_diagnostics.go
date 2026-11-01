package mesh

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"net/netip"
)

type underlayDiagnostics struct{ engineLabel string }

// Only the fork's authored primitive metadata is retained, never arbitrary
// formatted library messages, packet bodies or key material.
func (d *underlayDiagnostics) record(format string, args ...any) {
	if format != "paperboat underlay: direction=%s type=%d bytes=%d endpoint=%v failed=%t transport=%s" || len(args) != 6 {
		return
	}
	direction, ok := args[0].(string)
	if !ok || direction != "ingress" && direction != "egress" {
		return
	}
	kind, ok := args[1].(uint32)
	if !ok || kind < 1 || kind > 5 {
		return
	}
	size, ok := args[2].(int)
	if !ok || size < 4 {
		return
	}
	endpoint, ok := args[3].(netip.AddrPort)
	if !ok {
		return
	}
	failed, ok := args[4].(bool)
	if !ok {
		return
	}
	transport, ok := args[5].(string)
	if !ok || transport != "udp" && transport != "derp" {
		return
	}
	diagnosticlog.TryInfo("native underlay packet", "engine_label", d.engineLabel, "direction", direction, "packet_type", kind, "bytes", size, "endpoint_label", underlayLabel(endpoint.String()), "failed", failed, "path_category", transport)
}
func underlayLabel(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:8])
}
