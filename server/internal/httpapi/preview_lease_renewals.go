package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewv1"
)

// MaxPreviewLeaseRenewals bounds one batch: a machine renews all of its previews in
// ceil(N/100) requests instead of N (F21-168).
const MaxPreviewLeaseRenewals = 100

// previewLeaseRenewalWorkers is how many items of one batch run at once.
const previewLeaseRenewalWorkers = 8

type previewLeaseRenewalItem struct {
	PreviewID      string `json:"preview_id"`
	ETag           string `json:"etag"`
	IdempotencyKey string `json:"idempotency_key"`
	OwnerSessionID string `json:"owner_session_id"`
}

type previewLeaseRenewalResult struct {
	PreviewID string          `json:"preview_id"`
	Status    int             `json:"status"`
	ETag      string          `json:"etag,omitempty"`
	Preview   any             `json:"preview,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
}

// previewLeaseRenewBatch renews many leases owned by one authenticated machine. Each item is
// exactly one single-lease renewal (own ETag precondition, own idempotency key, same request
// hash), so one stale lease never fails the others.
func previewLeaseRenewBatch(service PreviewLeaseAPI, machineIdentities ...machineRequestVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		batchKey, err := previewtunnelapi.ParseIdempotencyKey(r.Header)
		if err != nil {
			writePreviewLeaseError(w, r, err)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 256<<10+1))
		if err != nil || len(raw) > 256<<10 {
			writePreviewLeaseError(w, r, fmt.Errorf("%w: request body is too large or unreadable", previewv1.ErrInvalidInput))
			return
		}
		var body struct {
			Renewals []previewLeaseRenewalItem `json:"renewals"`
		}
		if err := decodePreviewLeaseBody(raw, &body); err != nil {
			writePreviewLeaseError(w, r, err)
			return
		}
		if err := validatePreviewLeaseRenewals(body.Renewals); err != nil {
			writePreviewLeaseError(w, r, err)
			return
		}
		requestContext, ok := previewLeaseRequestContext(w, r, raw, batchKey, machineIdentities...)
		if !ok {
			return
		}
		// Items are independent single renewals, each in its own transaction. Run in turn, 100 of them took the whole
		// 15 s request timeout on staging (about 150 ms each), so batches ended with timed-out items and previews
		// lost their owner (F21-168); a few at a time they finish in seconds.
		results := make([]previewLeaseRenewalResult, len(body.Renewals))
		var wg sync.WaitGroup
		slots := make(chan struct{}, previewLeaseRenewalWorkers)
		for i, item := range body.Renewals {
			slots <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-slots; wg.Done() }()
				results[i] = renewPreviewLeaseItem(r, service, requestContext, item)
			}()
		}
		wg.Wait()
		w.Header().Set("Cache-Control", "no-store, no-transform")
		writeJSON(w, http.StatusOK, SuccessResponse{Data: struct {
			Results []previewLeaseRenewalResult `json:"results"`
		}{Results: results}})
	}
}

func validatePreviewLeaseRenewals(items []previewLeaseRenewalItem) error {
	if len(items) == 0 || len(items) > MaxPreviewLeaseRenewals {
		return fmt.Errorf("%w: a batch renews 1 to %d previews", previewv1.ErrInvalidInput, MaxPreviewLeaseRenewals)
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, dup := seen[item.PreviewID]; dup || item.PreviewID == "" {
			return fmt.Errorf("%w: each preview appears once in a batch", previewv1.ErrInvalidInput)
		}
		seen[item.PreviewID] = struct{}{}
	}
	return nil
}

func renewPreviewLeaseItem(r *http.Request, service PreviewLeaseAPI, requestContext previewtunnelapi.RequestContext, item previewLeaseRenewalItem) previewLeaseRenewalResult {
	fail := func(err error) previewLeaseRenewalResult {
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		writePreviewLeaseError(rec, r, err)
		// The writer's body is the {"error":{...}} envelope; the item carries the inner object.
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(rec.body.Bytes(), &envelope)
		return previewLeaseRenewalResult{PreviewID: item.PreviewID, Status: rec.status, Error: envelope.Error}
	}
	generation, err := previewtunnelapi.ParseIfMatch(http.Header{previewtunnelapi.IfMatchHeader: {item.ETag}}, "preview_lease", item.PreviewID)
	if err != nil {
		return fail(err)
	}
	key, err := previewtunnelapi.ParseIdempotencyKey(http.Header{previewtunnelapi.IdempotencyHeader: {item.IdempotencyKey}})
	if err != nil {
		return fail(err)
	}
	// The single endpoint hashes the body {"owner_session_id":...}; keep item records interchangeable.
	itemBody, err := json.Marshal(struct {
		OwnerSessionID string `json:"owner_session_id"`
	}{item.OwnerSessionID})
	if err != nil {
		return fail(err)
	}
	hash, err := previewtunnelapi.RequestHash(itemBody)
	if err != nil {
		return fail(fmt.Errorf("%w: request JSON is invalid", previewv1.ErrInvalidInput))
	}
	result, err := service.Renew(r.Context(), requestContext, item.PreviewID, previewv1.MutationRequest{
		ExpectedGeneration: generation, OwnerSessionID: item.OwnerSessionID, IdempotencyKey: key, RequestHash: hash,
	})
	if err != nil {
		return fail(err)
	}
	return previewLeaseRenewalResult{PreviewID: item.PreviewID, Status: http.StatusOK, ETag: result.ETag, Preview: result.Preview}
}

// bufferedResponse captures the error body writePreviewLeaseError would send for one item.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }
