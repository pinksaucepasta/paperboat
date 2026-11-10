package filetransfer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// F14-02 on Windows: during an update the local API named pipe is missing; the renewal must wait for the daemon, not fail the send.
func TestLocalDaemonRestartingRecognisesAMissingWindowsPipe(t *testing.T) {
	pipe := errors.Join(localapi.ErrTransportUnavailable, fmt.Errorf("connect named pipe: The system cannot find the file specified"))
	for name, test := range map[string]struct {
		err  error
		want bool
	}{
		"missing pipe":      {pipe, true},
		"wrapped renewal":   {fmt.Errorf("file transfer authorization renewal failed: %w", pipe), true},
		"permission":        {os.ErrPermission, false},
		"cancelled":         {context.Canceled, false},
		"other local error": {errors.New("disk full"), false},
	} {
		if got := localDaemonRestarting(test.err); got != test.want {
			t.Fatalf("%s: localDaemonRestarting = %v, want %v", name, got, test.want)
		}
	}
}
