package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

func TestThrottledSendReportsAllowanceAndProgressInBothModes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/billing/workspaces/personal" {
			http.NotFound(w, r)
			return
		}
		writeAPIData(t, w, map[string]any{
			"throttled": true, "throttle_mbit": 0.5, "allowance_period_end": "2026-12-01T00:00:00Z",
		})
	}))
	defer backend.Close()
	client := api.New(backend.URL, config.Credential{}, backend.Client())

	for _, mode := range []string{"human", "json"} {
		t.Run(mode, func(t *testing.T) {
			var output progressBuffer
			progress := newSendTransferOutput(&output, mode == "json", 4096)
			progress.interval = time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stop := progress.start(ctx, func(ctx context.Context) { progress.reportBandwidth(ctx, client, "") })
			t.Cleanup(func() { stop(false) })

			waitForLine := func(fragment string) string {
				t.Helper()
				for {
					for _, line := range strings.Split(output.String(), "\n") {
						if strings.Contains(line, fragment) {
							return line
						}
					}
					select {
					case <-ctx.Done():
						t.Fatalf("missing %q in %q", fragment, output.String())
					case <-time.After(time.Millisecond):
					}
				}
			}
			notice := waitForLine("throttled")
			for _, part := range []string{"allowance", "0.5", "own relay", "Billing", "reset"} {
				if !strings.Contains(notice, part) {
					t.Errorf("throttle notice omitted %q: %s", part, notice)
				}
			}

			if mode == "json" {
				var event struct {
					Event  string `json:"event"`
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal([]byte(notice), &event); err != nil || event.Event != "transfer_throttled" || event.Reason != "allowance_used" {
					t.Fatalf("invalid throttle event: %s (%v)", notice, err)
				}
				waitForLine(`"bytes_sent":0`)
				progress.committed("file-1", 1024)
				line := waitForLine(`"bytes_sent":1024`)
				var update struct {
					Event string `json:"event"`
					Total int64  `json:"total_bytes"`
				}
				if err := json.Unmarshal([]byte(line), &update); err != nil || update.Event != "transfer_progress" || update.Total != 4096 {
					t.Fatalf("invalid progress event: %s (%v)", line, err)
				}
			} else {
				waitForLine("0 B / 4.0 KiB  0%")
				progress.committed("file-1", 1024)
				waitForLine("1.0 KiB / 4.0 KiB  25%")
			}
		})
	}
}

type fakeSendClock struct{ at time.Time }

func (c *fakeSendClock) now() time.Time { return c.at }

func newFakeSend(out *progressBuffer, tty, jsonOutput bool, total int64) (*sendTransferOutput, *fakeSendClock) {
	clock := &fakeSendClock{at: time.Unix(1000, 0)}
	p := &sendTransferOutput{out: out, tty: tty, json: jsonOutput, total: total, now: clock.now, offsets: map[string]int64{}}
	p.samples = []sendSample{{at: clock.at}}
	return p, clock
}

func TestSendProgressLineShowsBytesPercentSpeedAndETA(t *testing.T) {
	var out progressBuffer
	p, clock := newFakeSend(&out, false, false, 10<<20)
	clock.at = clock.at.Add(2 * time.Second)
	p.committed("a", 2<<20)
	p.writeProgress(false)
	if got, want := out.String(), "2.0 MiB / 10.0 MiB  20%  1.0 MiB/s  ETA 0:08\n"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

func TestSendProgressSpeedFollowsTheRecentWindowNotTheWholeTransfer(t *testing.T) {
	var out progressBuffer
	p, clock := newFakeSend(&out, false, false, 100<<20)
	p.committed("a", 10<<20) // fast start: 10 MiB in the first second
	clock.at = clock.at.Add(time.Second)
	p.writeProgress(false)
	clock.at = clock.at.Add(20 * time.Second) // then a slow stretch: 1 MiB in 20 s
	p.committed("a", 11<<20)
	p.writeProgress(false)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.Contains(lines[1], "  51.2 KiB/s  ETA ") {
		t.Fatalf("speed after the slow stretch = %q", lines[1])
	}
}

func TestSendProgressOnATerminalRedrawsOneLineAndNamesThePlanThrottle(t *testing.T) {
	var out progressBuffer
	p, clock := newFakeSend(&out, true, false, 4<<20)
	p.throttle = 0.5
	clock.at = clock.at.Add(time.Second)
	p.committed("a", 1<<20)
	p.writeProgress(false)
	p.committed("a", 2<<20)
	clock.at = clock.at.Add(time.Second)
	p.writeProgress(false)
	got := out.String()
	if strings.Contains(got, "\n") || strings.Count(got, "\r") != 2 {
		t.Fatalf("a terminal line must be redrawn with \\r and no newline: %q", got)
	}
	for _, want := range []string{"2.0 MiB / 4.0 MiB  50%", "1.0 MiB/s", "ETA 0:02", "throttled to 0.5 Mbit/s by your plan"} {
		if !strings.Contains(got, want) {
			t.Errorf("terminal line lacks %q: %q", want, got)
		}
	}
}

func TestSendProgressWaitingMessageStartsOnAFreshLineAfterTheRedrawnOne(t *testing.T) {
	var out progressBuffer
	p, clock := newFakeSend(&out, true, false, 1<<20)
	clock.at = clock.at.Add(time.Second)
	p.writeProgress(false)
	p.waiting("Waiting for hp to finish another transfer.")
	if got := out.String(); !strings.Contains(got, "\nWaiting for hp to finish another transfer.\n") {
		t.Fatalf("waiting message shares the progress line: %q", got)
	}
}

func TestSendProgressFinishReportsTheWholeTransferAndEndsTheLine(t *testing.T) {
	var out progressBuffer
	p, clock := newFakeSend(&out, true, false, 3<<20)
	clock.at = clock.at.Add(3 * time.Second)
	p.committed("a", 1<<20) // the last checkpoint ran behind the data actually sent
	p.finish(true)
	got := out.String()
	if !strings.Contains(got, "3.0 MiB / 3.0 MiB  100%") || strings.Contains(got, "ETA") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("final line = %q", got)
	}
}

func TestSendProgressFinishAfterFailureOnlyEndsTheLine(t *testing.T) {
	var out progressBuffer
	p, clock := newFakeSend(&out, true, false, 3<<20)
	clock.at = clock.at.Add(time.Second)
	p.committed("a", 1<<20)
	p.writeProgress(false)
	p.finish(false)
	if got := out.String(); strings.Contains(got, "100%") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("failed transfer output = %q", got)
	}
}

func TestSendProgressJSONEventCarriesPercentSpeedETAAndThrottle(t *testing.T) {
	var out progressBuffer
	p, clock := newFakeSend(&out, false, true, 10<<20)
	p.throttle = 2
	clock.at = clock.at.Add(2 * time.Second)
	p.committed("a", 2<<20)
	p.writeProgress(false)
	var event struct {
		Event     string  `json:"event"`
		Sent      int64   `json:"bytes_sent"`
		Total     int64   `json:"total_bytes"`
		Percent   int     `json:"percent"`
		Speed     int64   `json:"bytes_per_second"`
		ETA       int64   `json:"eta_seconds"`
		Throttled bool    `json:"throttled"`
		PlanMbit  float64 `json:"plan_mbit_per_second"`
	}
	if err := json.Unmarshal([]byte(out.String()), &event); err != nil {
		t.Fatalf("%q: %v", out.String(), err)
	}
	if event.Event != "transfer_progress" || event.Sent != 2<<20 || event.Total != 10<<20 || event.Percent != 20 || event.Speed != 1<<20 || event.ETA != 8 || !event.Throttled || event.PlanMbit != 2 {
		t.Fatalf("event = %+v", event)
	}
}

func TestSendProgressWithoutADeclaredTotalShowsBytesAndSpeedOnly(t *testing.T) {
	var out progressBuffer
	p, clock := newFakeSend(&out, false, false, 0)
	clock.at = clock.at.Add(time.Second)
	p.committed("a", 512<<10)
	p.writeProgress(false)
	if got, want := out.String(), "512.0 KiB  512.0 KiB/s\n"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}
