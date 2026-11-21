package machineguard

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A guard operation that ran out of time reaches the daemon as a typed timeout it can retry, not a bare string.
func TestGuardDeadlineReachesTheClientAsATimeout(t *testing.T) {
	sent := fmt.Errorf("apply firewall rules: %w", context.DeadlineExceeded)
	got := responseError(response{Error: sent.Error(), ErrorCode: errorCode(sent)})
	if !errors.Is(got, context.DeadlineExceeded) || got.Error() != sent.Error() {
		t.Fatalf("got %v; want %q as a context.DeadlineExceeded", got, sent)
	}
	if errors.Is(responseError(response{Error: "refused", ErrorCode: errorCode(errors.New("refused"))}), context.DeadlineExceeded) {
		t.Fatal("an untyped error became a timeout")
	}
}

// Every platform's client waits longer than the guard's own per-operation deadline, so the guard's answer arrives.
func TestClientOutwaitsTheGuardOperationDeadline(t *testing.T) {
	for _, op := range []string{"acquire", "apply", "names", "certificate"} {
		if exchangeTimeout(op) <= operationTimeout {
			t.Fatalf("%s: client waits %s, guard allows %s", op, exchangeTimeout(op), operationTimeout)
		}
	}
}
