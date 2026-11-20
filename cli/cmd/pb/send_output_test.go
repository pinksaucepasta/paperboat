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
			progress := sendTransferOutput{out: &output, json: mode == "json", total: 4096, offsets: map[string]int64{}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			done := make(chan struct{})
			go func() {
				defer close(done)
				progress.reportBandwidth(ctx, client, "", time.Millisecond)
			}()
			t.Cleanup(func() { cancel(); <-done })

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
				waitForLine("0 of 4096 bytes sent")
				progress.committed("file-1", 1024)
				waitForLine("1024 of 4096 bytes sent")
			}
		})
	}
}
