package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// pb tunnel list answered the bare generic text (category local_io) when the server's 200 failed validation.
func TestUnsafeTunnelResponseNamesItsCauseAndTheNextStep(t *testing.T) {
	err := fmt.Errorf("list tunnels: %w", api.ErrUnsafeTunnelResponse)
	message := commandFailureMessage(err, classifyCommandFailure(err))
	if !strings.Contains(message, "rejected the tunnel answer") || !strings.Contains(message, "pb update") {
		t.Fatalf("message hides the cause: %q", message)
	}
}
