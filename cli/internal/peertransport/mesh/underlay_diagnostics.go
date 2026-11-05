package mesh

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"net/netip"
	"sync"
	"time"
)

// underlayInterval is the most often one kind of packet on one path is reported: every packet a session carries
// went to the disk ring (38-117 events a second on a loaded host, half of all retained events) and, with the
// ring's fsyncs, starved other pb commands. Later packets of the same kind inside the interval are counted and
// arrive in the next report as packets=N.
const underlayInterval = time.Second

// underlayKinds bounds the table: when it fills, it restarts, so a path change or an endpoint sweep cannot grow it.
const underlayKinds = 256

type underlayKey struct {
	direction, transport, endpoint string
	kind                           uint32
	failed                         bool
}

type underlayCount struct {
	reported time.Time
	packets  uint64
	bytes    uint64
}

type underlayDiagnostics struct {
	engineLabel string
	now         func() time.Time                  // nil: time.Now
	emit        func(message string, args ...any) // nil: diagnosticlog.TryInfo
	mu          sync.Mutex
	kinds       map[underlayKey]*underlayCount
}

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
	label := underlayLabel(endpoint.String())
	packets, bytes, report := d.count(underlayKey{direction: direction, transport: transport, endpoint: label, kind: kind, failed: failed}, size)
	if !report {
		return
	}
	emit := d.emit
	if emit == nil {
		emit = diagnosticlog.TryInfo
	}
	emit("native underlay packet", "engine_label", d.engineLabel, "direction", direction, "packet_type", kind, "bytes", bytes, "packets", packets, "endpoint_label", label, "failed", failed, "path_category", transport)
}

// count adds one packet and says whether this one carries a report: the first of its kind, or the first after
// underlayInterval since the last report. The report holds what was counted since.
func (d *underlayDiagnostics) count(key underlayKey, size int) (packets, bytes uint64, report bool) {
	now := time.Now
	if d.now != nil {
		now = d.now
	}
	at := now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.kinds == nil || len(d.kinds) >= underlayKinds {
		d.kinds = make(map[underlayKey]*underlayCount)
	}
	c := d.kinds[key]
	if c == nil {
		c = &underlayCount{}
		d.kinds[key] = c
	}
	c.packets++
	c.bytes += uint64(size)
	if !c.reported.IsZero() && at.Sub(c.reported) < underlayInterval {
		return 0, 0, false
	}
	packets, bytes = c.packets, c.bytes
	c.reported, c.packets, c.bytes = at, 0, 0
	return packets, bytes, true
}
func underlayLabel(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:8])
}
