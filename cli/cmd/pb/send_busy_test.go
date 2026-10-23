package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

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
