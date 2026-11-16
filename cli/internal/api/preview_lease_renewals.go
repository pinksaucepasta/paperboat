package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/shared/wirejson"
)

// MaxPreviewLeaseRenewals is the server's batch limit (F21-168).
const MaxPreviewLeaseRenewals = 100

// PreviewLeaseRenewal is one lease of a batch: the same inputs as RenewPreviewLease.
type PreviewLeaseRenewal struct {
	Lease          PreviewLease
	OwnerSessionID string
	IdempotencyKey string
}

// PreviewLeaseRenewalResult is the outcome of one batch item; Err is its own typed API error.
type PreviewLeaseRenewalResult struct {
	Lease PreviewLease
	Err   error
}

// RenewPreviewLeases renews up to MaxPreviewLeaseRenewals leases of one machine in one request, so
// N previews cost ceil(N/100) requests per renewal interval instead of N. Items fail alone; the
// returned error means the whole request failed. batchKey identifies this request (machine proof).
func (c *Client) RenewPreviewLeases(ctx context.Context, items []PreviewLeaseRenewal, batchKey string) ([]PreviewLeaseRenewalResult, error) {
	if len(items) == 0 || len(items) > MaxPreviewLeaseRenewals {
		return nil, fmt.Errorf("%w: a batch renews 1 to %d previews", ErrPreviewLeaseInvalid, MaxPreviewLeaseRenewals)
	}
	if err := validatePreviewLeaseIdempotencyKey(batchKey); err != nil {
		return nil, err
	}
	type wireItem struct {
		PreviewID      string `json:"preview_id"`
		ETag           string `json:"etag"`
		IdempotencyKey string `json:"idempotency_key"`
		OwnerSessionID string `json:"owner_session_id"`
	}
	wire := make([]wireItem, len(items))
	for i, item := range items {
		if err := validateMutationLease(item.Lease, item.IdempotencyKey); err != nil {
			return nil, err
		}
		if strings.TrimSpace(item.OwnerSessionID) == "" {
			return nil, fmt.Errorf("%w: owner session is required", ErrPreviewLeaseInvalid)
		}
		wire[i] = wireItem{item.Lease.ID, item.Lease.ETag, item.IdempotencyKey, item.OwnerSessionID}
	}
	var response struct {
		Results []struct {
			PreviewID string          `json:"preview_id"`
			Status    int             `json:"status"`
			ETag      string          `json:"etag"`
			Preview   json.RawMessage `json:"preview"`
			Error     json.RawMessage `json:"error"`
		} `json:"results"`
	}
	err := c.doRequestMeta(ctx, http.MethodPost, "/v1/preview-lease-renewals", struct {
		Renewals []wireItem `json:"renewals"`
	}{wire}, &response, http.Header{"Idempotency-Key": []string{batchKey}, "Accept-Encoding": []string{"identity"}}, true, nil)
	if err != nil {
		return nil, err
	}
	if len(response.Results) != len(items) {
		return nil, fmt.Errorf("%w: batch answered %d of %d renewals", ErrPreviewLeaseInvalid, len(response.Results), len(items))
	}
	out := make([]PreviewLeaseRenewalResult, len(items))
	for i, item := range items {
		got := response.Results[i]
		if got.PreviewID != item.Lease.ID {
			return nil, fmt.Errorf("%w: batch answer order does not match the request", ErrPreviewLeaseInvalid)
		}
		out[i] = renewalResult(item.Lease, got.Status, got.ETag, got.Preview, got.Error)
	}
	return out, nil
}

func renewalResult(lease PreviewLease, status int, etag string, preview, errBody json.RawMessage) PreviewLeaseRenewalResult {
	if status != http.StatusOK {
		var detail apiEnvelope
		_ = json.Unmarshal([]byte(`{"error":`+string(errBody)+`}`), &detail)
		return PreviewLeaseRenewalResult{Err: &APIError{Status: status, Code: detail.Error.Code, Message: apiErrorMessageFor(detail.Error.Code, status, detail.Error.Details), ServerMessage: BoundedServerMessage(detail.Error.Message), SupportReference: detail.Error.SupportReference, Details: detail.Error.Details}}
	}
	var renewed PreviewLease
	if err := wirejson.Decode(preview, &renewed); err != nil {
		return PreviewLeaseRenewalResult{Err: &ResponseDecodeError{Err: err}}
	}
	renewed.ETag = strings.TrimSpace(etag)
	if err := validatePreviewLease(renewed); err != nil {
		return PreviewLeaseRenewalResult{Err: err}
	}
	if _, err := previewLeaseETag(renewed.ID, renewed.ETag); err != nil {
		return PreviewLeaseRenewalResult{Err: err}
	}
	if !samePreviewLeaseIdentity(lease, renewed) {
		return PreviewLeaseRenewalResult{Err: errors.Join(ErrPreviewLeaseInvalid, errors.New("renewal changed lease identity"))}
	}
	return PreviewLeaseRenewalResult{Lease: renewed}
}
