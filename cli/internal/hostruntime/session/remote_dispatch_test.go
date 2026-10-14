package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// Value-only service methods must not turn a failed authenticated hop into a
// successful empty list or discard the caller's cancellation.
func TestRemoteDispatchPreservesReadFailureAndCancellation(t *testing.T) {
	for _, op := range []string{"List", "ResourceCounts"} {
		t.Run(op, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			proxy, err := NewRemoteManager(func(received context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
				return nil, received.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ServeRemoteCall(ctx, proxy, op, json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled proxy returned successful read: %v", err)
			}
			failure := ErrOwnerUnavailable
			proxy, err = NewRemoteManager(func(context.Context, string, json.RawMessage) (json.RawMessage, error) { return nil, failure })
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ServeRemoteCall(context.Background(), proxy, op, json.RawMessage(`{}`)); !errors.Is(err, failure) {
				t.Fatalf("proxy hid owner failure: %v", err)
			}
		})
	}
}
