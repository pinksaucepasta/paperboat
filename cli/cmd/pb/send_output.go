package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"golang.org/x/term"
)

// Like apt, curl and rsync: on a terminal one line is redrawn in place every second; otherwise (a pipe, a log, CI)
// a plain line every transferProgressInterval; with --json one transfer_progress event per interval.
const (
	sendTerminalInterval = time.Second
	sendSpeedWindow      = 10 * time.Second
)

type sendSample struct {
	at   time.Time
	sent int64
}

type sendTransferOutput struct {
	out   io.Writer
	json  bool
	tty   bool // redraw one line in place
	total int64
	now   func() time.Time // tests only; nil means time.Now
	// interval overrides how often a line is written; tests only.
	interval time.Duration

	mu       sync.Mutex
	lastWait string
	offsets  map[string]int64
	samples  []sendSample // the last sendSpeedWindow of progress, oldest first, starting with the start
	throttle float64      // the plan's reduced speed in Mbit/s once the allowance is used up, else 0
	drawn    int          // width of the in-place line on screen
	complete bool         // the transfer finished: report all of total, whatever the last checkpoints said
}

func newSendTransferOutput(out io.Writer, jsonOutput bool, total int64) *sendTransferOutput {
	file, isFile := out.(*os.File)
	return &sendTransferOutput{
		out: out, json: jsonOutput, tty: !jsonOutput && isFile && term.IsTerminal(int(file.Fd())), total: total,
		offsets: make(map[string]int64),
	}
}

func (p *sendTransferOutput) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
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
		p.endLine()
		fmt.Fprintln(p.out, message)
	}
}

// reportBandwidth tells the sender once when the plan slows relayed transfers; every progress line then says so too.
func (p *sendTransferOutput) reportBandwidth(ctx context.Context, client *api.Client, teamID string) {
	lookup, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	bandwidth, err := client.WorkspaceBandwidth(lookup, teamID)
	if err != nil || !bandwidth.Throttled || bandwidth.SlowMbit <= 0 || ctx.Err() != nil {
		return
	}

	const recovery = "Use a direct connection or your own relay, add bandwidth or change plan in Billing, or wait for the allowance to reset."
	message := fmt.Sprintf("The workspace used its traffic allowance. Transfers through Paperboat relays are throttled to the plan's reduced speed (%g Mbit/s shared across relays).", bandwidth.SlowMbit)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.throttle = bandwidth.SlowMbit
	if p.json {
		_ = json.NewEncoder(p.out).Encode(map[string]any{
			"schema_version": "1.0", "event": "transfer_throttled", "reason": "allowance_used",
			"relay_only": true, "plan_mbit_per_second": bandwidth.SlowMbit, "allowance_resets_at": bandwidth.AllowanceEnd,
			"message": message, "recovery": recovery,
		})
		return
	}
	p.endLine()
	fmt.Fprintln(p.out, message, recovery)
}

// start reports progress until the returned stop is called with whether the transfer succeeded.
// lookup, when set, runs beside it (the plan's throttle notice).
func (p *sendTransferOutput) start(ctx context.Context, lookup func(context.Context)) (stop func(succeeded bool)) {
	interval := p.interval
	if interval == 0 {
		interval = transferProgressInterval
		if p.tty {
			interval = sendTerminalInterval
		}
	}
	p.mu.Lock()
	p.samples = []sendSample{{at: p.clock()}}
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	var done sync.WaitGroup
	if lookup != nil {
		done.Add(1)
		go func() { defer done.Done(); lookup(ctx) }()
	}
	done.Add(1)
	go func() {
		defer done.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.writeProgress(false)
			}
		}
	}()
	return func(succeeded bool) {
		cancel()
		done.Wait()
		p.finish(succeeded)
	}
}

// finish writes the last line: the whole transfer when it succeeded, or just the end of the redrawn line.
func (p *sendTransferOutput) finish(succeeded bool) {
	if succeeded {
		p.mu.Lock()
		p.complete = true
		p.mu.Unlock()
		p.writeProgress(true)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endLine()
}

func (p *sendTransferOutput) sentLocked() int64 {
	if p.complete && p.total > 0 {
		return p.total
	}
	var sent int64
	for _, offset := range p.offsets {
		sent += offset
	}
	return sent
}

// endLine leaves the in-place line on screen and moves to the next one.
func (p *sendTransferOutput) endLine() {
	if p.tty && p.drawn > 0 {
		fmt.Fprintln(p.out)
		p.drawn = 0
	}
}

func (p *sendTransferOutput) writeProgress(final bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	sent := p.sentLocked()
	if len(p.samples) == 0 {
		p.samples = []sendSample{{at: now}}
	}
	for len(p.samples) > 1 && now.Sub(p.samples[1].at) >= sendSpeedWindow {
		p.samples = p.samples[1:]
	}
	oldest := p.samples[0]
	var speed float64
	if elapsed := now.Sub(oldest.at).Seconds(); elapsed > 0 {
		speed = float64(sent-oldest.sent) / elapsed
	}
	p.samples = append(p.samples, sendSample{at: now, sent: sent})

	percent, eta := -1, time.Duration(-1)
	if p.total > 0 {
		percent = int(min(sent, p.total) * 100 / p.total)
		if speed > 0 && sent < p.total {
			eta = time.Duration(float64(p.total-sent) / speed * float64(time.Second))
		}
	}
	if p.json {
		event := map[string]any{"schema_version": "1.0", "event": "transfer_progress", "bytes_sent": sent, "total_bytes": p.total, "bytes_per_second": int64(speed)}
		if percent >= 0 {
			event["percent"] = percent
		}
		if eta >= 0 {
			event["eta_seconds"] = int64(eta.Round(time.Second).Seconds())
		}
		if p.throttle > 0 {
			event["throttled"], event["plan_mbit_per_second"] = true, p.throttle
		}
		_ = json.NewEncoder(p.out).Encode(event)
		return
	}
	line := p.humanLine(sent, percent, speed, eta, final)
	if !p.tty {
		fmt.Fprintln(p.out, line)
		return
	}
	pad := strings.Repeat(" ", max(0, p.drawn-len(line)))
	fmt.Fprint(p.out, "\r"+line+pad)
	p.drawn = len(line)
}

func (p *sendTransferOutput) humanLine(sent int64, percent int, speed float64, eta time.Duration, final bool) string {
	var line strings.Builder
	line.WriteString(formatTransferBytes(sent))
	if p.total > 0 {
		fmt.Fprintf(&line, " / %s  %d%%", formatTransferBytes(p.total), percent)
	}
	fmt.Fprintf(&line, "  %s/s", formatTransferBytes(int64(speed)))
	if !final && p.total > 0 {
		line.WriteString("  ETA " + formatTransferETA(eta))
	}
	if p.throttle > 0 {
		fmt.Fprintf(&line, "  (throttled to %g Mbit/s by your plan)", p.throttle)
	}
	return line.String()
}

func formatTransferBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value, unit := float64(n), "B"
	for _, next := range []string{"KiB", "MiB", "GiB", "TiB"} {
		if value < 1024 {
			break
		}
		value, unit = value/1024, next
	}
	return fmt.Sprintf("%.1f %s", value, unit)
}

func formatTransferETA(eta time.Duration) string {
	if eta < 0 {
		return "--:--"
	}
	seconds := int64(eta.Round(time.Second).Seconds())
	if hours := seconds / 3600; hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, seconds/60%60, seconds%60)
	}
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}
