package serve

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/preview"
)

type unreachableLeaseClient struct {
	creates atomic.Int32
	cancel  context.CancelFunc
}

func (c *unreachableLeaseClient) Create(context.Context, preview.LeaseRequest) (preview.Lease, error) {
	if c.creates.Add(1) == 1 {
		time.AfterFunc(100*time.Millisecond, c.cancel) // Ctrl-C during the retry wait
	}
	return preview.Lease{}, &api.APIError{Status: http.StatusConflict, Code: "machine_unreachable", RetryAfter: 2 * time.Second}
}
func (*unreachableLeaseClient) Renew(context.Context, preview.Lease, string) (preview.Lease, error) {
	return preview.Lease{}, nil
}
func (*unreachableLeaseClient) Stop(context.Context, preview.Lease, string) error { return nil }

type unusedCarrier struct{ preview.Carrier }

// StartForeground detaches the session from Ctrl-C, so the create retry wait must still observe it.
func TestStartForegroundCancelDuringCreateRetryReturnsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &unreachableLeaseClient{cancel: cancel}
	_, err := StartForeground(ctx, ForegroundConfig{
		Name: "preview", Target: &preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"},
		LeaseClient: client, Carrier: unusedCarrier{}, OwnerMachineID: "machine_cli",
		OwnerSessionID: "session_cli", OwnerSessionKind: preview.OwnerSessionForeground, AccessMode: "public",
	})
	var refused *api.APIError
	if !errors.Is(err, context.Canceled) || errors.As(err, &refused) || client.creates.Load() != 1 {
		t.Fatalf("err=%v creates=%d, want context.Canceled after one create", err, client.creates.Load())
	}
}
