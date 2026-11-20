package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

type sendTransferOutput struct {
	out   io.Writer
	json  bool
	total int64

	mu       sync.Mutex
	lastWait string
	offsets  map[string]int64
}

func (p *sendTransferOutput) committed(id string, offset int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.offsets[id] = max(p.offsets[id], offset)
}

func (p *sendTransferOutput) waiting(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.json && p.lastWait != message {
		p.lastWait = message
		fmt.Fprintln(p.out, message)
	}
}

func (p *sendTransferOutput) reportBandwidth(ctx context.Context, client *api.Client, teamID string, interval time.Duration) {
	lookup, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	bandwidth, err := client.WorkspaceBandwidth(lookup, teamID)
	if err != nil || !bandwidth.Throttled || bandwidth.SlowMbit <= 0 || ctx.Err() != nil {
		return
	}

	const recovery = "Use a direct connection or your own relay, add bandwidth or change plan in Billing, or wait for the allowance to reset."
	message := fmt.Sprintf("The workspace used its traffic allowance. Transfers through Paperboat relays are throttled to the plan's reduced speed (%g Mbit/s shared across relays).", bandwidth.SlowMbit)
	p.mu.Lock()
	if p.json {
		_ = json.NewEncoder(p.out).Encode(map[string]any{
			"schema_version": "1.0", "event": "transfer_throttled", "reason": "allowance_used",
			"relay_only": true, "plan_mbit_per_second": bandwidth.SlowMbit, "allowance_resets_at": bandwidth.AllowanceEnd,
			"message": message, "recovery": recovery,
		})
	} else {
		fmt.Fprintln(p.out, message, recovery)
	}
	p.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.writeProgress()
		}
	}
}

func (p *sendTransferOutput) writeProgress() {
	p.mu.Lock()
	defer p.mu.Unlock()
	var sent int64
	for _, offset := range p.offsets {
		sent += offset
	}
	if p.json {
		_ = json.NewEncoder(p.out).Encode(map[string]any{
			"schema_version": "1.0", "event": "transfer_progress", "bytes_sent": sent, "total_bytes": p.total,
		})
	} else {
		fmt.Fprintf(p.out, "Transfer in progress (%d of %d bytes sent).\n", sent, p.total)
	}
}
