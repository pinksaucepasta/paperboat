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

// An owner outlives an update, so it serves calls from a newer worker that carry fields it does not know: rejecting
// them failed the attach and the user got a new shell after an official→dev update.
func TestRemoteDispatchAcceptsNewerWorkerFields(t *testing.T) {
	proxy, err := NewRemoteManager(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"V0":{},"V1":false}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(`{"Sessionid":"s","Attachmentid":"a","Addedbyanewerbuild":7}`)
	if _, err := ServeRemoteCall(context.Background(), proxy, "Next", body); err != nil {
		t.Fatalf("owner refused a newer worker's call: %v", err)
	}
}
