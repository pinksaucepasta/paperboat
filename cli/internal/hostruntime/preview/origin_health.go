package preview

import (
	"context"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/originhealth"
)

// originReportHub carries origin-health changes from the carrier that dials the app to the
// dispatch layer that owns the lease and may report them to the control plane. Keyed by lease ID.
type originReportHub struct {
	mu    sync.Mutex
	sinks map[string]func(originhealth.Event)
}

var previewOriginReports = &originReportHub{sinks: make(map[string]func(originhealth.Event))}

// register installs the sink for leaseID and returns its removal.
func (h *originReportHub) register(leaseID string, sink func(originhealth.Event)) func() {
	h.mu.Lock()
	h.sinks[leaseID] = sink
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.sinks, leaseID)
		h.mu.Unlock()
	}
}

func (h *originReportHub) report(leaseID string, event originhealth.Event) {
	h.mu.Lock()
	sink := h.sinks[leaseID]
	h.mu.Unlock()
	if sink != nil {
		sink(event)
	}
}

// originReporter turns tracker events into at-most-one-at-a-time lease readiness reports. A
// recovery is reported only after an outage was, so the readiness already published when the
// preview became ready is not repeated.
type originReporter struct {
	mu           sync.Mutex
	reportedDown bool
	send         func(context.Context, bool) error
}

func (r *originReporter) handle(ctx context.Context, event originhealth.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	up := event.State == originhealth.Up
	if up && !r.reportedDown {
		return
	}
	reportCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := r.send(reportCtx, up); err != nil {
		return // the next transition or repeat reports again
	}
	r.reportedDown = !up
}
