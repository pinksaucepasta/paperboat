package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
)

func TestConnectNamesAnUnreachableTargetInsteadOfTheGenericFailure(t *testing.T) {
	refusal := &api.APIError{Status: http.StatusConflict, Code: resolver.MachineUnreachableCode, Message: "studio-mac is not reachable from Paperboat right now: it may be offline, or cut off from the Paperboat service. Check it with `pb status`, then retry"}
	err := fmt.Errorf("connect: %w", refusal)
	message := commandFailureMessage(err, classifyCommandFailure(err))
	if !strings.Contains(message, "studio-mac is not reachable from Paperboat right now") || strings.Contains(message, "could not finish") {
		t.Fatalf("message = %q", message)
	}
}
