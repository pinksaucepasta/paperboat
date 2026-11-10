package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// A daemon that is not listening is an operational failure with the "unreachable" wording, not an unexpected one: every
// local API request (not only streams) reports a dial failure joined with ErrTransportUnavailable. (A missing socket is
// ENOENT, which the classifier already treated as unexpected before; it is deliberately unchanged.)
func TestADaemonThatIsNotListeningStaysOperational(t *testing.T) {
	for name, cause := range map[string]error{
		"refused socket": &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
	} {
		err := fmt.Errorf("Get \"http://paperboat.local/v1/snapshot\": %w", errors.Join(localapi.ErrTransportUnavailable, cause))
		if kind := classifyCommandFailure(err).kind; kind != commandOperational {
			t.Fatalf("%s: kind = %v, want commandOperational (%v)", name, kind, commandOperational)
		}
	}
}
