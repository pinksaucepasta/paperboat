package offlineaudit

import (
	"encoding/json"
	"testing"
	"time"
)

// Uploaded under the machine proof and decoded strictly: the optional members stay absent when empty.
func TestEventWire(t *testing.T) {
	raw, err := json.Marshal(Event{EventID: "e", Kind: "k", Capability: "exec", PeerEndpointID: "p", PeerAccountID: "a", OperationID: "o", OccurredAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Generation: 4})
	want := `{"event_id":"e","kind":"k","capability":"exec","peer_endpoint_id":"p","peer_account_id":"a","operation_id":"o","occurred_at":"2030-01-01T00:00:00Z","generation":4}`
	if err != nil || string(raw) != want {
		t.Fatalf("event = %s, %v", raw, err)
	}
}
