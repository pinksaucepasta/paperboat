package selfhost

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// ServeTLS failed for real and the context was cancelled at the same moment: both cases of the select are ready and the
// failure must be reported whichever one select picks. (-count=50)
func TestAwaitServeReportsARealServeErrorThatArrivesWithTheCancel(t *testing.T) {
	failure := errors.New("listener failed")
	for range 50 {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		done := make(chan error, 1)
		done <- failure
		if err := awaitServe(ctx, &http.Server{}, done); !errors.Is(err, failure) {
			t.Fatalf("awaitServe = %v, want the ServeTLS failure", err)
		}
	}
}

func TestAwaitServeTreatsAServerClosedAsACleanStop(t *testing.T) {
	for range 50 {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		done := make(chan error, 1)
		done <- http.ErrServerClosed
		if err := awaitServe(ctx, &http.Server{}, done); err != nil {
			t.Fatalf("awaitServe = %v, want nil", err)
		}
	}
}
