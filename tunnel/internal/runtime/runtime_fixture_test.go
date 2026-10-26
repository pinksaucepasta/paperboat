package runtime

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
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
