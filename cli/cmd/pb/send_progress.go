package main

import (
	"context"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// transferProgressInterval is the most often a sending transfer reports committed bytes; state changes
// (paused, sending again, done, failed) are reported at once.
const transferProgressInterval = 5 * time.Second

// transferProgress reports a send's progress on its approved team-inbox request, so the console shows it
// (PLAN 3b File transfer modes). Reports are sent in the background, one at a time, and never slow or fail the
// transfer: a newer report replaces an unsent one, and a failed report is dropped.
type transferProgress struct {
	report    func(context.Context, api.TransferProgress) error
	mode      string
	mu        sync.Mutex
	committed map[string]int64
	state     string
	reason    string
	sentAt    time.Time
	pending   *api.TransferProgress
	sending   bool
	done      chan struct{}
}

func newTransferProgress(client *api.Client, requestID, mode string) *transferProgress {
	return &transferProgress{mode: mode, committed: map[string]int64{}, done: make(chan struct{}), report: func(ctx context.Context, progress api.TransferProgress) error {
		return client.ReportTransferProgress(ctx, requestID, progress)
	}}
}

func (p *transferProgress) Committed(transferID string, offset int64) {
	p.mu.Lock()
	p.committed[transferID] = offset
	// Bytes committed after a pause mean the transfer is sending again: report that at once.
	due := p.state == "paused" || p.state == "sending" && time.Since(p.sentAt) >= transferProgressInterval
	p.mu.Unlock()
	if due {
		p.set("sending", "")
	}
}

func (p *transferProgress) Waiting(string) { p.set("paused", "no_eligible_path") }

// set records the state and reports it.
func (p *transferProgress) set(state, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var total int64
	for _, offset := range p.committed {
		total += offset
	}
	p.state, p.reason, p.sentAt = state, reason, time.Now()
	p.pending = &api.TransferProgress{TransferMode: p.mode, TransferState: state, PauseReason: reason, CommittedBytes: total}
	if !p.sending {
		p.sending = true
		go p.drain()
	}
}

func (p *transferProgress) drain() {
	for {
		p.mu.Lock()
		next := p.pending
		p.pending = nil
		if next == nil {
			p.sending = false
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = p.report(ctx, *next)
		cancel()
	}
}

// finish reports the final state with every byte committed and waits briefly for the report to go out.
func (p *transferProgress) finish(state string, total int64) {
	p.mu.Lock()
	p.committed = map[string]int64{"all": total}
	p.mu.Unlock()
	p.set(state, "")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		idle := !p.sending
		p.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
