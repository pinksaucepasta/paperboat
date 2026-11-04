package tailnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// pbvm4 run19 logged only "fmt.wrapError" for a stopped relay edge.
func TestRelayFailureTypeNamesTheCause(t *testing.T) {
	for want, err := range map[string]error{
		"relay_refused_1": fmt.Errorf("prepare: %w", derpquic.ErrAdmission),
		"deadline":        fmt.Errorf("connect: %w", context.DeadlineExceeded),
		"net.OpError":     fmt.Errorf("magicsock: connect DERP region 1: %w", &net.OpError{Op: "read", Err: errors.New("connection refused")}),
	} {
		if got := relayFailureType(err); got != want {
			t.Errorf("relayFailureType(%v) = %q, want %q", err, got, want)
		}
	}
}
