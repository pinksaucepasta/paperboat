package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat/server/internal/previewv1"
)

// renewalAPI renews every lease except those listed in stale, which fail like a lease that changed.
type renewalAPI struct {
	*fakePreviewLeaseAPI
	stale     map[string]bool
	renewed   []string
	mutations []previewv1.MutationRequest
	mu        sync.Mutex
	delay     time.Duration
	running   int
	peak      int
}

func (f *renewalAPI) Renew(_ context.Context, _ previewtunnelapi.RequestContext, id string, mutation previewv1.MutationRequest) (previewv1.RenewResult, error) {
	f.mu.Lock()
	f.renewed = append(f.renewed, id)
	f.mutations = append(f.mutations, mutation)
	f.running++
	f.peak = max(f.peak, f.running)
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	f.running--
	f.mu.Unlock()
	if f.stale[id] {
		return previewv1.RenewResult{}, previewtunnelstore.ErrGenerationConflict
	}
	return previewv1.RenewResult{Preview: previewv1.Preview{ID: id}, ETag: renewalETag(id, mutation.ExpectedGeneration+1)}, nil
}

func renewalETag(id string, generation int64) string {
	return fmt.Sprintf(`"ptv1:preview_lease:%s:%d"`, base64.RawURLEncoding.EncodeToString([]byte(id)), generation)
}

func renewalBody(ids ...string) string {
	items := make([]string, len(ids))
	for i, id := range ids {
		items[i] = fmt.Sprintf(`{"preview_id":%q,"etag":%q,"idempotency_key":"key_%s","owner_session_id":"session_1"}`, id, renewalETag(id, 1), id)
	}
	return `{"renewals":[` + strings.Join(items, ",") + `]}`
}

func renewalRequest(body string) *http.Request {
	request := previewLeaseHandlerRequest(http.MethodPost, "/v1/preview-lease-renewals", body)
	request.Header.Set("Idempotency-Key", "batch_1")
	return request
}

type renewalResponse struct {
	Data struct {
		Results []struct {
			PreviewID string          `json:"preview_id"`
			Status    int             `json:"status"`
			ETag      string          `json:"etag"`
			Error     json.RawMessage `json:"error"`
		} `json:"results"`
	} `json:"data"`
}

func TestPreviewLeaseRenewBatchRenewsEachLeaseAndKeepsOrder(t *testing.T) {
	service := &renewalAPI{fakePreviewLeaseAPI: &fakePreviewLeaseAPI{}, stale: map[string]bool{"prv_2": true}}
	recorder := httptest.NewRecorder()
	previewLeaseRenewBatch(service).ServeHTTP(recorder, renewalRequest(renewalBody("prv_1", "prv_2", "prv_3")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response renewalResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	got := response.Data.Results
	if len(got) != 3 || got[0].PreviewID != "prv_1" || got[1].PreviewID != "prv_2" || got[2].PreviewID != "prv_3" {
		t.Fatalf("results = %+v", got)
	}
	if got[0].Status != 200 || got[0].ETag != renewalETag("prv_1", 2) || got[2].Status != 200 {
		t.Fatalf("renewed items = %+v / %+v", got[0], got[2])
	}
	// A stale lease fails alone with the error the single endpoint writes for it.
	single := httptest.NewRecorder()
	writePreviewLeaseError(single, renewalRequest("{}"), previewtunnelstore.ErrGenerationConflict)
	var wantEnvelope struct {
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(single.Body.Bytes(), &wantEnvelope)
	if got[1].Status != http.StatusPreconditionFailed || string(got[1].Error) != string(wantEnvelope.Error) || !strings.Contains(string(got[1].Error), "generation_conflict") {
		t.Fatalf("stale item = %+v, want status 412 with %s", got[1], wantEnvelope.Error)
	}
	byKey := map[string]previewv1.MutationRequest{}
	for _, mutation := range service.mutations {
		byKey[mutation.IdempotencyKey] = mutation
	}
	first, ok := byKey["key_prv_1"]
	if len(service.mutations) != 3 || !ok || first.OwnerSessionID != "session_1" || first.ExpectedGeneration != 1 {
		t.Fatalf("item mutations = %+v", service.mutations)
	}
	want, _ := previewtunnelapi.RequestHash([]byte(`{"owner_session_id":"session_1"}`))
	if first.RequestHash != want {
		t.Fatal("item request hash differs from the single renew endpoint's, so idempotency records would not interchange")
	}
}

func TestPreviewLeaseRenewBatchRefusesBadBatches(t *testing.T) {
	ids := make([]string, MaxPreviewLeaseRenewals+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("prv_%d", i)
	}
	for name, body := range map[string]string{
		"empty":     `{"renewals":[]}`,
		"too many":  renewalBody(ids...),
		"duplicate": renewalBody("prv_1", "prv_1"),
		"unknown":   `{"renewals":[],"extra":1}`,
	} {
		service := &renewalAPI{fakePreviewLeaseAPI: &fakePreviewLeaseAPI{}}
		recorder := httptest.NewRecorder()
		previewLeaseRenewBatch(service).ServeHTTP(recorder, renewalRequest(body))
		if recorder.Code != http.StatusBadRequest || len(service.renewed) != 0 {
			t.Fatalf("%s: status = %d renewed %v body = %s", name, recorder.Code, service.renewed, recorder.Body.String())
		}
	}
	// The limit itself is accepted.
	service := &renewalAPI{fakePreviewLeaseAPI: &fakePreviewLeaseAPI{}}
	recorder := httptest.NewRecorder()
	previewLeaseRenewBatch(service).ServeHTTP(recorder, renewalRequest(renewalBody(ids[:MaxPreviewLeaseRenewals]...)))
	if recorder.Code != http.StatusOK || len(service.renewed) != MaxPreviewLeaseRenewals {
		t.Fatalf("100 items: status = %d renewed %d", recorder.Code, len(service.renewed))
	}
}

func TestPreviewLeaseRenewBatchBindsTheMachineProofToTheBatch(t *testing.T) {
	body := renewalBody("prv_1", "prv_2")
	service := &renewalAPI{fakePreviewLeaseAPI: &fakePreviewLeaseAPI{}}
	verifier := validPreviewMachineVerifier()
	request := machinePreviewMutationRequest(http.MethodPost, "/v1/preview-lease-renewals", body)
	request.Header.Del("If-Match")
	recorder := httptest.NewRecorder()
	previewLeaseRenewBatch(service, verifier).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || len(service.renewed) != 2 {
		t.Fatalf("status = %d renewed %v body = %s", recorder.Code, service.renewed, recorder.Body.String())
	}
	if verifier.method != http.MethodPost || verifier.path != "/v1/preview-lease-renewals" || string(verifier.body) != body {
		t.Fatalf("proof binding = %q %q %q", verifier.method, verifier.path, verifier.body)
	}

	service = &renewalAPI{fakePreviewLeaseAPI: &fakePreviewLeaseAPI{}}
	request = machinePreviewMutationRequest(http.MethodPost, "/v1/preview-lease-renewals", body)
	request.Header.Set("Idempotency-Key", "other_batch") // proof says op_1
	recorder = httptest.NewRecorder()
	previewLeaseRenewBatch(service, validPreviewMachineVerifier()).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || len(service.renewed) != 0 {
		t.Fatalf("wrong batch key: status = %d renewed %v", recorder.Code, service.renewed)
	}
}

// F21-168: a batch of 100 renewals that each take 50 ms finishes in about a second (a few at a time), in request
// order, never with more than the worker bound at once.
func TestPreviewLeaseRenewBatchRunsItemsConcurrentlyWithinTheBound(t *testing.T) {
	ids := make([]string, MaxPreviewLeaseRenewals)
	for i := range ids {
		ids[i] = fmt.Sprintf("prv_%d", i)
	}
	service := &renewalAPI{fakePreviewLeaseAPI: &fakePreviewLeaseAPI{}, delay: 50 * time.Millisecond, stale: map[string]bool{"prv_7": true}}
	recorder := httptest.NewRecorder()
	started := time.Now()
	previewLeaseRenewBatch(service).ServeHTTP(recorder, renewalRequest(renewalBody(ids...)))
	took := time.Since(started)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if took > 3*time.Second {
		t.Fatalf("100 renewals of 50 ms took %s; run in turn they take 5 s, with %d workers about %s", took, previewLeaseRenewalWorkers, 50*time.Millisecond*100/previewLeaseRenewalWorkers)
	}
	if service.peak < 2 || service.peak > previewLeaseRenewalWorkers {
		t.Fatalf("peak concurrency %d, want between 2 and %d", service.peak, previewLeaseRenewalWorkers)
	}
	var response renewalResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || len(response.Data.Results) != len(ids) {
		t.Fatalf("results %d err %v", len(response.Data.Results), err)
	}
	for i, got := range response.Data.Results {
		want := http.StatusOK
		if ids[i] == "prv_7" {
			want = http.StatusPreconditionFailed
		}
		if got.PreviewID != ids[i] || got.Status != want {
			t.Fatalf("result %d = %+v, want %s with status %d", i, got, ids[i], want)
		}
	}
}
