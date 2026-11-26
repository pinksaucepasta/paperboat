//go:build darwin || linux || windows

package runtime

import (
	"slices"
	"testing"
)

// Smoke 2026-11-26: pb exec -- pb send took the terminal-only host-local path and failed.
func TestExecutionEnvironmentOmitsHostLocalDeliveryCredentials(t *testing.T) {
	agent := []string{"HOME=/h", "PAPERBOAT_FILE_TRANSFER_ENDPOINT=http://127.0.0.1:1/v1/file-transfers",
		"PAPERBOAT_FILE_TRANSFER_STAGING_ENDPOINT=http://127.0.0.1:1/v1/local-file-transfers",
		"PAPERBOAT_RUNTIME_AGENT_TOKEN_FILE=/s/agent/token", "PAPERBOAT_WORKSPACE_ROOT=/w"}
	got := executionEnvironment(agent)
	want := []string{"HOME=/h", "PAPERBOAT_FILE_TRANSFER_ENDPOINT=http://127.0.0.1:1/v1/file-transfers", "PAPERBOAT_WORKSPACE_ROOT=/w"}
	if !slices.Equal(got, want) || len(agent) != 5 {
		t.Fatalf("execution environment = %q, want %q", got, want)
	}
}
