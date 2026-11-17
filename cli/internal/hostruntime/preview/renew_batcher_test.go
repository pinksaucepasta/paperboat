package preview

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

func batchTestLease(id string) api.PreviewLease {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) // fixed: CreatedAt is part of a lease identity
	return api.PreviewLease{
		Schema: api.PreviewTunnelSchemaV1, Kind: "preview_lease", ID: id, AccountID: "acct_1", ActorID: "actor_1",
		OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "local_lease",
		Target: api.PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public",
		Endpoint: "https://" + id + ".preview.example.test", EndpointState: "ready", LeaseDeadline: time.Now().Add(30 * time.Minute).UTC(),
		State: "ready", AllocationState: "ready", EdgeState: "ready", OriginState: "ready", CreatedAt: now, LastRenewedAt: now,
		ETag: batchTestETag(id, 1),
	}
}

func batchTestETag(id string, generation int) string {
	return fmt.Sprintf(`"ptv1:preview_lease:%s:%d"`, base64.RawURLEncoding.EncodeToString([]byte(id)), generation)
}

// F21-168: a machine with many previews sends a few batched requests per renewal interval, not one each.
func TestAPILeaseClientRenewsManyPreviewsInAFewRequests(t *testing.T) {
	const previews = 250
	var batches, singles, largest atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/preview-lease-renewals" {
			singles.Add(1)
			http.NotFound(w, r)
			return
		}
		batches.Add(1)
		var body struct {
			Renewals []struct {
				PreviewID string `json:"preview_id"`
			} `json:"renewals"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if n := int64(len(body.Renewals)); n > largest.Load() {
			largest.Store(n)
		}
		results := []map[string]any{}
		for _, item := range body.Renewals {
			results = append(results, map[string]any{"preview_id": item.PreviewID, "status": 200, "etag": batchTestETag(item.PreviewID, 2), "preview": batchTestLease(item.PreviewID)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"results": results}})
	}))
	defer server.Close()
	leases, err := NewAPILeaseClient(api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < previews; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("prv_%d", i)
			lease := leaseFromAPI(batchTestLease(id))
			renewed, err := leases.Renew(context.Background(), lease, "operation_"+id)
			if err != nil || renewed.ID != id || renewed.ETag != batchTestETag(id, 2) {
				t.Errorf("%s renewed %+v err %v", id, renewed, err)
			}
		}(i)
	}
	wg.Wait()
	if singles.Load() != 0 || batches.Load() > 4 || largest.Load() > api.MaxPreviewLeaseRenewals {
		t.Fatalf("%d previews took %d batches (largest %d) and %d single renewals; want <= 4 batches of <= %d", previews, batches.Load(), largest.Load(), singles.Load(), api.MaxPreviewLeaseRenewals)
	}
}

func TestRenewBatcherNeverNamesALeaseTwiceInOneRequest(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	b := newRenewBatcher(func(_ context.Context, items []api.PreviewLeaseRenewal, _ string) ([]api.PreviewLeaseRenewalResult, error) {
		seen := map[string]bool{}
		out := make([]api.PreviewLeaseRenewalResult, len(items))
		for i, item := range items {
			if seen[item.Lease.ID] {
				t.Errorf("lease %s twice in one batch", item.Lease.ID)
			}
			seen[item.Lease.ID] = true
			out[i] = api.PreviewLeaseRenewalResult{Lease: item.Lease}
		}
		mu.Lock()
		sizes = append(sizes, len(items))
		mu.Unlock()
		return out, nil
	})
	b.window = 20 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ { // the same lease retried while its first renewal is still queued
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.renew(context.Background(), api.PreviewLeaseRenewal{Lease: batchTestLease("prv_1"), IdempotencyKey: "k"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(sizes) != 3 {
		t.Fatalf("batch sizes = %v, want three single-lease requests", sizes)
	}
}

func TestRenewBatcherFailsEveryItemOnARequestError(t *testing.T) {
	want := fmt.Errorf("control unavailable")
	b := newRenewBatcher(func(context.Context, []api.PreviewLeaseRenewal, string) ([]api.PreviewLeaseRenewalResult, error) {
		return nil, want
	})
	b.window = 10 * time.Millisecond
	if _, err := b.renew(context.Background(), api.PreviewLeaseRenewal{Lease: batchTestLease("prv_1")}); err != want {
		t.Fatalf("err = %v", err)
	}
}

func TestRenewBatcherReturnsWhenTheCallerGivesUp(t *testing.T) {
	b := newRenewBatcher(func(context.Context, []api.PreviewLeaseRenewal, string) ([]api.PreviewLeaseRenewalResult, error) {
		return nil, nil
	})
	b.window = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := b.renew(ctx, api.PreviewLeaseRenewal{Lease: batchTestLease("prv_1")}); err != context.DeadlineExceeded {
		t.Fatalf("err = %v", err)
	}
}
