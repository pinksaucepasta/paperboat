package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func renewalTestLease(id string) PreviewLease {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) // fixed: CreatedAt is part of a lease identity
	return PreviewLease{
		Schema: PreviewTunnelSchemaV1, Kind: "preview_lease", ID: id, AccountID: "acct_1", ActorID: "actor_1",
		OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "local_lease",
		Target: PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public",
		Endpoint: "https://" + id + ".preview.example.test", EndpointState: "ready", LeaseDeadline: time.Now().Add(30 * time.Minute).UTC(),
		State: "ready", AllocationState: "ready", EdgeState: "ready", OriginState: "ready", CreatedAt: now, LastRenewedAt: now,
		ETag: renewalTestETag(id, 1),
	}
}

func renewalTestETag(id string, generation int) string {
	return `"ptv1:preview_lease:` + base64.RawURLEncoding.EncodeToString([]byte(id)) + `:` + string(rune('0'+generation)) + `"`
}

func TestRenewPreviewLeasesAnswersEachItemAlone(t *testing.T) {
	type wireItem struct {
		PreviewID      string `json:"preview_id"`
		ETag           string `json:"etag"`
		IdempotencyKey string `json:"idempotency_key"`
		OwnerSessionID string `json:"owner_session_id"`
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/preview-lease-renewals" || r.Header.Get("Idempotency-Key") != "batch-key" {
			t.Errorf("request = %s %s key %q", r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"))
		}
		proof, _ := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if r.Header.Get("X-Paperboat-Machine-Identity") != "machine-token" || !strings.HasPrefix(string(proof), "batch-key|POST|/v1/preview-lease-renewals|") {
			t.Errorf("batch is not machine-signed with its own key: identity %q proof %q", r.Header.Get("X-Paperboat-Machine-Identity"), proof)
		}
		var body struct {
			Renewals []wireItem `json:"renewals"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Renewals) != 3 {
			t.Fatalf("body = %+v err %v", body, err)
		}
		results := []map[string]any{}
		for i, item := range body.Renewals {
			if item.OwnerSessionID != "session_1" || item.IdempotencyKey != "key-"+item.PreviewID {
				t.Errorf("item = %+v", item)
			}
			if i == 1 {
				results = append(results, map[string]any{"preview_id": item.PreviewID, "status": 412, "error": map[string]any{"code": "generation_conflict", "message": "The preview changed; refresh it and retry."}})
				continue
			}
			renewed := renewalTestLease(item.PreviewID)
			results = append(results, map[string]any{"preview_id": item.PreviewID, "status": 200, "etag": renewalTestETag(item.PreviewID, 2), "preview": renewed})
		}
		writePreviewLeaseEnvelope(t, w, map[string]any{"results": results})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "access-token"}, server.Client())
	client.SetMachineAuth(machineAuthTestSource{})
	items := []PreviewLeaseRenewal{}
	for _, id := range []string{"prv_a", "prv_b", "prv_c"} {
		items = append(items, PreviewLeaseRenewal{Lease: renewalTestLease(id), OwnerSessionID: "session_1", IdempotencyKey: "key-" + id})
	}
	results, err := client.RenewPreviewLeases(context.Background(), items, "batch-key")
	if err != nil || len(results) != 3 || requests != 1 {
		t.Fatalf("results %+v err %v requests %d", results, err, requests)
	}
	if results[0].Err != nil || results[0].Lease.ETag != renewalTestETag("prv_a", 2) || results[2].Err != nil || results[2].Lease.ID != "prv_c" {
		t.Fatalf("renewed items = %+v / %+v", results[0], results[2])
	}
	var apiErr *APIError
	if !errors.As(results[1].Err, &apiErr) || apiErr.Status != 412 || apiErr.Code != "generation_conflict" {
		t.Fatalf("stale item error = %v", results[1].Err)
	}
}

func TestRenewPreviewLeasesRefusesAnOversizedBatchBeforeSending(t *testing.T) {
	client := New("http://127.0.0.1:1", config.Credential{AccessToken: "t"}, nil)
	items := make([]PreviewLeaseRenewal, MaxPreviewLeaseRenewals+1)
	if _, err := client.RenewPreviewLeases(context.Background(), items, "batch-key"); !errors.Is(err, ErrPreviewLeaseInvalid) {
		t.Fatalf("err = %v", err)
	}
}
