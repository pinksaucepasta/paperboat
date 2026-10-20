package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// An unknown machine name says so and names the command that lists machines, before any connection.
func TestExecUnknownMachineSaysItWasNotFound(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", jsonOutput), func(t *testing.T) {
			var descriptors atomic.Int32
			configPath, paths, cfg := execCommandBrokerFixture(t, &descriptors, false)
			server, err := localapi.NewServer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- server.Run(ctx) }()
			defer stopLocalDaemonReadinessServer(t, cancel, done)
			waitForCommandSocket(t, paths.SocketPath)
			var stdout, stderr bytes.Buffer
			args := []string{"--config", configPath, "--no-customization", "exec", "no-such-machine"}
			if jsonOutput {
				args = append(args, "--json")
			}
			args = append(args, "--", "fixture-command")
			if code := run(t.Context(), args, &stdout, &stderr); code == 0 {
				t.Fatal("unknown machine reported success")
			}
			text := stderr.String()
			if jsonOutput {
				var event struct {
					ErrorCode string `json:"error_code"`
					Detail    string `json:"detail"`
					Changed   bool   `json:"changed"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &event); err != nil {
					t.Fatal(err)
				}
				text = event.Detail
				if event.ErrorCode != "machine_not_found" || event.Changed {
					t.Fatalf("outcome=%+v", event)
				}
			}
			if !strings.Contains(text, "No machine has that name or ID") || !strings.Contains(text, "pb machine list") {
				t.Fatalf("unknown machine guidance missing: %q", text)
			}
			if descriptors.Load() != 0 {
				t.Fatal("descriptor requested for an unknown machine")
			}
		})
	}
}
