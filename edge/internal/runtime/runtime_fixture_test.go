package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
)

// newTestCarrierServer is a carrier server for identity that authorizes every stream.
func newTestCarrierServer(t *testing.T, identity datacarrier.Identity) *datacarrier.Server {
	t.Helper()
	config := datacarrier.DefaultConfig()
	config.Identity = identity
	config.Authorize = datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error { return nil })
	server, err := datacarrier.NewServerWithSession(context.Background(), newTestCarrierSession(), config)
	if err != nil {
		t.Fatalf("new carrier server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

// handleInBackground runs w.Handle for server and, when the test ends, closes the server and waits for
// Handle to return. An unjoined Handle logs runtime_route_detached into whichever test runs next
// (TestEndpointRemovalsAreLoggedWithReason swaps the global slog default).
func handleInBackground(t *testing.T, ctx context.Context, w *RuntimeCarrierWorker, server *datacarrier.Server) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Handle(ctx, server) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(runtimeWaitBound):
			t.Error("Handle did not return after its carrier closed")
		}
	})
}
