package main

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelenrollment"
	"strings"
	"testing"
)

// Lane H repro, 2026-10-26: `pb access machine` failed with only "Paperboat could not finish the command"
// (error chain joinError:wrapError:errorString). The step, its cause and the next action are now shown.
func TestAccessMachineFailureNamesTheStepAndCause(t *testing.T) {
	err := errors.Join(&accessMachineStepError{step: "opening authorized access to hp port 22", action: "Run `pb doctor` and retry.", cause: errors.New("native private grant refused: native_private_unavailable")}, nil)
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	for _, want := range []string{"opening authorized access to hp port 22", "native_private_unavailable", "pb doctor"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}
	if strings.Contains(message, "could not finish the command") {
		t.Fatalf("the generic message hid the cause: %q", message)
	}
}

// Staging, 2026-10-26: connector add showed local_io when the server refused the connector's carrier.
func TestConnectorAddNamesTheServerRefusal(t *testing.T) {
	err := connectorEnrollFailure(&tunnelenrollment.CarrierBootstrapError{StatusCode: 503, Code: "carrier_unavailable"})
	message := commandFailureMessage(err, classifyCommandFailure(err))
	for _, want := range []string{"starting the connector's carrier", "carrier_unavailable", "edge pool"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}
}
