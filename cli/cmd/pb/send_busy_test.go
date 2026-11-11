package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
)

// A receiver that already holds as many unfinished transfers as it accepts refuses with 429
// resource_limit (Lane M, F14-02 on lanem-b2): the send names the machine and how to clear it,
// instead of a bare rate_limited.
func TestADestinationAtItsTransferLimitSaysHowToClearIt(t *testing.T) {
	err := destinationBusy("lanem-b2", fmt.Errorf("send batch: %w", &filetransfer.Error{Code: "resource_limit", StatusCode: 429}))
	message := commandFailureMessage(err, classifyCommandFailure(err))
	result := classifyCLIJSONError(err)
	if !strings.Contains(message, "lanem-b2 is already receiving") || !strings.Contains(message, "pb send cancel <transfer-id> --on lanem-b2") || result.Code != "destination_busy" {
		t.Fatalf("message=%q code=%q", message, result.Code)
	}
	if other := errors.New("other"); destinationBusy("m", other) != other {
		t.Fatal("an unrelated error changed")
	}
}

// A running send whose destination is removed or revoked (the renewal answers 404 user_machine_not_found) stops at
// once and says why, instead of waiting for a machine that never comes back (F14-02 review of 1f6c7685f).
func TestARemovedDestinationStopsTheSendAndSaysSo(t *testing.T) {
	cause := fmt.Errorf("file transfer authorization renewal failed: %w", &api.APIError{Status: 404, Code: "user_machine_not_found"})
	err := destinationBusy("macl", destinationStorageFull("macl", destinationGone("macl", cause)))
	message := commandFailureMessage(err, classifyCommandFailure(err))
	result := classifyCLIJSONError(err)
	if message != "macl was removed or revoked, so the transfer stopped; nothing more is sent to it." || result.Code != "destination_removed" || result.Retryable {
		t.Fatalf("message=%q code=%q retryable=%v", message, result.Code, result.Retryable)
	}
	offline := &api.APIError{Status: 409, Code: "machine_offline"}
	if destinationGone("macl", offline) != error(offline) {
		t.Fatal("an offline machine was reported as removed")
	}
}
