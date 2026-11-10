package filetransfer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
		"missing pipe":    {pipe, true},
		"wrapped renewal": {fmt.Errorf("file transfer authorization renewal failed: %w", pipe), true},
		// The shape that failed an update on winpool-11: the renewal's pipe dial error carries no transport wrapper.
		"bare missing pipe":   {fmt.Errorf("file transfer authorization renewal failed: connect named pipe: %w", &fs.PathError{Op: "open", Path: `\\.\pipe\paperboat-local-api-e655ce475da61ffc2e916d1d`, Err: fs.ErrNotExist}), true},
		"missing source file": {&fs.PathError{Op: "open", Path: `C:\Users\x\file.bin`, Err: fs.ErrNotExist}, false},
		"pipe access refused": {&fs.PathError{Op: "open", Path: `\\.\pipe\paperboat-local-api-x`, Err: fs.ErrPermission}, false},
		"permission":          {os.ErrPermission, false},
		"cancelled":           {context.Canceled, false},
		"other local error":   {errors.New("disk full"), false},
	} {
		if got := localDaemonRestarting(test.err); got != test.want {
			t.Fatalf("%s: localDaemonRestarting = %v, want %v", name, got, test.want)
		}
	}
}

// An unknown lease handle is not a restarting daemon: waiting on the same handle never helps.
func TestUnknownLeaseHandleIsNotARestartingDaemon(t *testing.T) {
	unknown := &localapi.RemoteError{StatusCode: 404, Code: localapi.FileTransferUnknownHandleCode}
	if localDaemonRestarting(unknown) || !transferLeaseUnknown(fmt.Errorf("stream: %w", unknown)) {
		t.Fatal("an unknown handle was classified as a restarting daemon")
	}
	if transferLeaseUnknown(&localapi.RemoteError{StatusCode: 404, Code: "other"}) || transferLeaseUnknown(&localapi.RemoteError{StatusCode: 503, Code: localapi.FileTransferUnknownHandleCode}) {
		t.Fatal("only a 404 with the unknown-handle code is a lost lease")
	}
	if !retryableTransferFailure(&transferLeaseLost{unknown}) {
		t.Fatal("a lost lease is not retried with a new lease")
	}
}
