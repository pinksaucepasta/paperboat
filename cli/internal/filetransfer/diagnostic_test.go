package filetransfer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

func TestHTTPFailureProjectsStatusWithoutResponseTextOrIDs(t *testing.T) {
	err := &Error{Code: "server_private_code", Message: "private response payload", StatusCode: 429, RequestID: "request_private_identifier"}
	fault := errorreport.ProjectFault(context.Background(), "pb", "file_transfer", "command", "unexpected_cli_failure", err)
	if fault.Stage != "command" || fault.Code != "file_transfer_failed" || fault.HTTPStatus != 429 || fault.Cause != "rate_limited" {
		t.Fatalf("projected transfer fault=%+v", fault)
	}
	serialized := fmt.Sprintf("%+v", fault)
	for _, private := range []string{"private response payload", "request_private_identifier", "server_private_code"} {
		if strings.Contains(serialized, private) {
			t.Fatalf("projected transfer fault retained %q: %s", private, serialized)
		}
	}
}

func TestDeliveryTimeoutHasStaticPhaseAndPreservesDeadline(t *testing.T) {
	err := fileTransferPhaseFailure("delivery", context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) || err.Error() != "file delivery timed out" {
		t.Fatalf("delivery error=%v", err)
	}
	fault := errorreport.ProjectFault(context.Background(), "pb", "file_transfer", "command", "unexpected_cli_failure", err)
	if fault.Stage != "delivery" || fault.Code != "file_transfer_failed" || fault.Cause != "deadline_exceeded" {
		t.Fatalf("projected delivery fault=%+v", fault)
	}
}

func TestDeliveryContextErrorLeavesUserCancellationUnwrapped(t *testing.T) {
	cause := errors.New("caller stopped delivery")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	err := deliveryContextError(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("cancellation status or cause lost: %v", err)
	}
}
