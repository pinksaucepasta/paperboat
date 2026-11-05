package machineguard

import (
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Gateway refusal reasons: a fixed set, so the counters stay bounded.
const (
	refusalNotFound      = "not_found"
	refusalAmbiguous     = "ambiguous"
	refusalInconsistent  = "inconsistent"
	refusalUnsupported   = "unsupported"
	refusalNoDaemon      = "no_daemon"
	refusalLookupFailed  = "lookup_failed"
	refusalBusy          = "busy"
	refusalHandoffFailed = "handoff_failed"
)

// gatewayRefusalLogEvery bounds the log to one line per reason in this interval.
const gatewayRefusalLogEvery = time.Minute

// gatewayRefusals counts every gateway connection the guard did not hand to a daemon, by reason, so an empty
// answer is visible as a counted refusal (RuntimeStatus.GatewayRefusals) and in the log.
var gatewayRefusals = struct {
	sync.Mutex
	counts map[string]uint64
	logged map[string]time.Time
}{counts: map[string]uint64{}, logged: map[string]time.Time{}}

func countGatewayRefusal(reason string, port int, err error) {
	now := time.Now()
	gatewayRefusals.Lock()
	gatewayRefusals.counts[reason]++
	total := gatewayRefusals.counts[reason]
	log := now.Sub(gatewayRefusals.logged[reason]) >= gatewayRefusalLogEvery
	if log {
		gatewayRefusals.logged[reason] = now
	}
	gatewayRefusals.Unlock()
	if log {
		slog.Warn("gateway connection refused", "reason", reason, "port", port, "total", total, "error", err)
	}
}

// attributionRefusalReason names the reason of an attribution failure; anything else is a failed lookup.
func attributionRefusalReason(err error) string {
	var refused attributionError
	if !errors.As(err, &refused) {
		return refusalLookupFailed
	}
	switch refused {
	case errAttributionNotFound:
		return refusalNotFound
	case errAttributionAmbiguous:
		return refusalAmbiguous
	case errAttributionInconsistent:
		return refusalInconsistent
	case errAttributionUnsupported:
		return refusalUnsupported
	case errAttributionNoDaemon:
		return refusalNoDaemon
	}
	return refusalLookupFailed
}

func gatewayRefusalSnapshot() map[string]uint64 {
	gatewayRefusals.Lock()
	defer gatewayRefusals.Unlock()
	if len(gatewayRefusals.counts) == 0 {
		return nil
	}
	out := make(map[string]uint64, len(gatewayRefusals.counts))
	for reason, n := range gatewayRefusals.counts {
		out[reason] = n
	}
	return out
}
