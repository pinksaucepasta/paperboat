package preview

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

type scriptedCreateClient struct {
	errs []error
	keys []string
}

func (c *scriptedCreateClient) Create(_ context.Context, request LeaseRequest) (Lease, error) {
	c.keys = append(c.keys, request.IdempotencyKey)
	if len(c.errs) == 0 {
		return Lease{ID: "preview_ok"}, nil
	}
	err := c.errs[0]
	c.errs = c.errs[1:]
	return Lease{}, err
}
func (*scriptedCreateClient) Renew(context.Context, Lease, string) (Lease, error) {
	return Lease{}, nil
}
func (*scriptedCreateClient) Stop(context.Context, Lease, string) error { return nil }

func unreachable() error {
	return &api.APIError{Status: http.StatusConflict, Code: "machine_unreachable", RetryAfter: 2 * time.Second}
}

func retryConfig(client LeaseClient, stderr *bytes.Buffer, waits *[]time.Duration) SessionConfig {
	return SessionConfig{LeaseClient: client, Stderr: stderr, IdempotencyKey: "operation_first",
		sleep: func(_ context.Context, d time.Duration) bool { *waits = append(*waits, d); return true }}
}

// L41: a retryable 409 machine_unreachable is retried with a new key per try.
func TestCreateRetriesMachineUnreachableWithNewKeys(t *testing.T) {
	client := &scriptedCreateClient{errs: []error{unreachable(), unreachable()}}
	var stderr bytes.Buffer
	var waits []time.Duration
	config := retryConfig(client, &stderr, &waits)
	request := LeaseRequest{IdempotencyKey: config.IdempotencyKey}
	lease, err := createLeaseRetryingUnreachable(context.Background(), &config, &request)
	if err != nil || lease.ID != "preview_ok" || len(client.keys) != 3 {
		t.Fatalf("lease=%+v err=%v keys=%v", lease, err, client.keys)
	}
	if client.keys[0] == client.keys[1] || client.keys[1] == client.keys[2] || client.keys[0] != "operation_first" {
		t.Fatalf("keys not renewed: %v", client.keys)
	}
	if strings.Count(stderr.String(), "retrying") != 2 || len(waits) != 2 || waits[0] != 2*time.Second {
		t.Fatalf("stderr=%q waits=%v", stderr.String(), waits)
	}
}

func TestCreateGivesUpAfterThreeUnreachableTries(t *testing.T) {
	client := &scriptedCreateClient{errs: []error{unreachable(), unreachable(), unreachable(), unreachable()}}
	var stderr bytes.Buffer
	var waits []time.Duration
	config := retryConfig(client, &stderr, &waits)
	request := LeaseRequest{IdempotencyKey: config.IdempotencyKey}
	_, err := createLeaseRetryingUnreachable(context.Background(), &config, &request)
	var refused *api.APIError
	if !errors.As(err, &refused) || refused.Code != "machine_unreachable" || len(client.keys) != 3 {
		t.Fatalf("err=%v keys=%v", err, client.keys)
	}
}

func TestCreateDoesNotRetryRouteMoving(t *testing.T) {
	moving := &api.APIError{Status: http.StatusConflict, Code: "machine_route_moving", RetryAfter: 2 * time.Second}
	client := &scriptedCreateClient{errs: []error{moving}}
	var stderr bytes.Buffer
	var waits []time.Duration
	config := retryConfig(client, &stderr, &waits)
	request := LeaseRequest{IdempotencyKey: config.IdempotencyKey}
	if _, err := createLeaseRetryingUnreachable(context.Background(), &config, &request); !errors.Is(err, moving) || len(client.keys) != 1 || stderr.Len() != 0 {
		t.Fatalf("err=%v keys=%v stderr=%q", err, client.keys, stderr.String())
	}
}
