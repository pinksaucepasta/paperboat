package telemetry

import "sync/atomic"

// Process-wide byte totals, read by the node heartbeat (the metric counters cannot be read back). Both traffic
// producers (public HTTPS and the connector carrier) record here next to the Prometheus counter.
var ingressBytes, egressBytes atomic.Uint64

// RecordTraffic adds bytes carried in one direction ("ingress" or "egress"); anything else is ignored.
func RecordTraffic(direction string, n uint64) {
	switch direction {
	case "ingress":
		ingressBytes.Add(n)
	case "egress":
		egressBytes.Add(n)
	}
}

// TrafficTotals is the bytes carried since this process started.
func TrafficTotals() (ingress, egress uint64) { return ingressBytes.Load(), egressBytes.Load() }
