package filetransfer

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// pipeStepFailure has the shape of pipeauth's dial failure: a step name over the pipe's PathError.
type pipeStepFailure struct{ cause error }

func (e pipeStepFailure) Error() string { return "connect named pipe" }
func (e pipeStepFailure) Unwrap() error { return e.cause }

// The renewal of a send started before an update on Windows: PrepareFileTransfer's dial fails on the missing pipe, and
// localapi types it exactly like this (localDialFailure: ErrTransportUnavailable joined with the pipe dial failure).
// It must be retried as a restarting daemon, not become a refusal that fails the transfer (update_survival, winpool-11).
func TestRenewalOverAMissingWindowsPipeWaitsForTheDaemon(t *testing.T) {
	first := &nativeTestLease{expires: time.Now().Add(time.Minute)}
	initial := NativeGrant{Endpoint: "http://fixture/v1/file-transfers", Auth: Auth{Token: "first", ExpiresAt: first.expires}, Binding: testBinding(), Lease: first}
	missing := errors.Join(localapi.ErrTransportUnavailable, pipeStepFailure{&fs.PathError{Op: "open", Path: `\\.\pipe\paperboat-local-api-x`, Err: fs.ErrNotExist}})
	client, err := NewRenewableNativeClient(initial, func(context.Context) (NativeGrant, error) {
		return NativeGrant{}, missing
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RefreshAuth(t.Context())
	var waiting *localDaemonUnavailable
	var refused *nativeAuthorizationFailure
	if !errors.As(err, &waiting) || errors.As(err, &refused) {
		t.Fatalf("renewal over a missing pipe: %T %v; want a retried localDaemonUnavailable", err, err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
}
