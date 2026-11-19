package preview

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// First preview on a fresh pair never went live: when no edge served the route within endpointReadyTimeout the carrier
// returned a plain error, the session ended and the lease stayed orphaned. The wait now ends as a retryable carrier
// error (the session reconnects and waits again), still reporting why.
func TestReadyWhenServedGivesUpAsRetryable(t *testing.T) {
	err := readyWhenServed(context.Background(), 50*time.Millisecond, Lease{}, func(Lease) error {
		return readinessFailed("POST", "http_status", http.StatusConflict, ErrDispatchNotServed, nil)
	})
	if !retryableCarrier(err) || !errors.Is(err, ErrDispatchNotServed) {
		t.Fatalf("readyWhenServed = %v, want a retryable carrier error wrapping ErrDispatchNotServed", err)
	}
}
