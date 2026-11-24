package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

// One refused carrier was also reported as ERROR http_server_failed (Europe edge 2026-11-24).
func TestCarrierConnectionFailureIsNotAServerFailure(t *testing.T) {
	if mode := os.Getenv("PB_CARRIER_FAILURE_HELPER"); mode != "" {
		reporter, err := reporting.New("paperboat-edge")
		if err != nil {
			t.Fatal(err)
		}
		defer reporter.Close()
		failure := error(datacarrier.RefusalUnknownMachine)
		if mode == "server" {
			failure = datacarrier.ErrCarrierHTTPServer
		}
		reportCarrierFailure(context.Background(), reporter, fmt.Errorf("carrier: %w", failure))
		return
	}
	for mode, want := range map[string]int{"connection": 0, "server": 1} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCarrierConnectionFailureIsNotAServerFailure$")
		command.Env = append(os.Environ(), "PB_CARRIER_FAILURE_HELPER="+mode, "PAPERBOAT_SENTRY_ENABLED=false")
		var stderr bytes.Buffer
		command.Stderr = &stderr
		err := command.Run()
		cancel()
		if err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, stderr.String())
		}
		if got := strings.Count(stderr.String(), `"http_server_failed"`); got != want {
			t.Fatalf("%s: http_server_failed events=%d, want %d:\n%s", mode, got, want, stderr.String())
		}
	}
}
