package preview

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/httptransport"
)

// AttachmentClient is the authenticated host-side HTTP boundary for the
// server previewattachment contract. It does not store or return bearer or
// private material; TokenSource and ProofSource retain ownership of those.
type AttachmentClient struct {
	base             *url.URL
	tokens           TokenSource
	identities       TokenSource
	proofs           ProofSource
	client           *http.Client
	maxResponseBytes int64
	admissionPoll    time.Duration
}

type AttachmentClientConfig struct {
	ControlURL   string
	AllowedHosts []string
	// Tokens supplies the renewable bearer credential. When omitted, the
	// identity source is reused for deployments where the machine credential
	// intentionally authenticates both headers.
	Tokens                TokenSource
	Identities            TokenSource
	Proofs                ProofSource
	Transport             http.RoundTripper
	Timeout               time.Duration
	MaxResponseBytes      int64
	AdmissionPollInterval time.Duration
}

func NewAttachmentClient(config AttachmentClientConfig) (*AttachmentClient, error) {
	if config.Timeout == 0 {
		config.Timeout = 15 * time.Second
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = attachmentResponseMaxBytes
	}
	if config.AdmissionPollInterval == 0 {
		config.AdmissionPollInterval = attachmentAdmissionPoll
	}
	base, err := url.Parse(strings.TrimSpace(config.ControlURL))
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "" && base.Path != "/" || config.Identities == nil || config.Proofs == nil || config.Timeout <= 0 || config.Timeout > 30*time.Second || config.MaxResponseBytes < 1 || config.MaxResponseBytes > attachmentResponseMaxBytes || config.AdmissionPollInterval <= 0 || config.AdmissionPollInterval > 5*time.Second {
		return nil, ErrAttachmentClientInvalid
	}
	allowed := false
	for _, host := range config.AllowedHosts {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(host), "."), strings.TrimSuffix(base.Hostname(), ".")) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, ErrAttachmentClientInvalid
	}
	transport := config.Transport
	if transport == nil {
		transport = httptransport.Default()
	}
	base.Path, base.RawPath = "", ""
	return &AttachmentClient{base: base, tokens: config.Tokens, identities: config.Identities, proofs: config.Proofs, maxResponseBytes: config.MaxResponseBytes, admissionPoll: config.AdmissionPollInterval, client: &http.Client{Transport: errorreport.TransportOperation(transport, base.String(), "preview_attachment"), Timeout: config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrAttachmentClientInvalid }}}, nil
}

// Allocate posts one exact operation. Replaying the same request is safe at
// the server idempotency boundary; changing any binding field is rejected
// before a proof or HTTP request is sent.
func (c *AttachmentClient) Allocate(ctx context.Context, request AttachmentRequest) (Attachment, error) {
	if c == nil || c.base == nil || ctx == nil {
		return Attachment{}, ErrAttachmentClientInvalid
	}
	if err := request.Validate(); err != nil {
		return Attachment{}, err
	}
	if strings.TrimSpace(request.LeaseETag) == "" {
		return Attachment{}, api.ErrPreviewLeaseETagRequired
	}
	if err := api.ValidatePreviewLeaseETag(request.PreviewID, request.LeaseETag); err != nil {
		return Attachment{}, fmt.Errorf("%w: lease ETag: %v", ErrAttachmentBinding, err)
	}
	path := "/v1/previews/" + url.PathEscape(request.PreviewID) + "/carrier-attachment"
	body, err := json.Marshal(request)
	if err != nil || len(body) == 0 || len(body) > attachmentRequestMaxBytes {
		return Attachment{}, fmt.Errorf("%w: request body is invalid", ErrAttachmentClientInvalid)
	}
	tokenSource := c.tokens
	if tokenSource == nil {
		tokenSource = c.identities
	}
	token, err := tokenSource.Token(ctx)
	if err != nil || token == "" || len(token) > attachmentIdentityMaxBytes || hasAttachmentControl(token) {
		return Attachment{}, errors.Join(ErrAttachmentClientUnavailable, err)
	}
	identity, err := c.identities.Token(ctx)
	if err != nil || identity == "" || len(identity) > attachmentIdentityMaxBytes || hasAttachmentControl(identity) {
		return Attachment{}, errors.Join(ErrAttachmentClientUnavailable, err)
	}
	proof, err := c.proofs.Proof(ctx, request.OperationID, http.MethodPost, path, body)
	if err != nil || len(proof) == 0 || len(proof) > attachmentProofMaxBytes {
		return Attachment{}, errors.Join(ErrAttachmentClientUnavailable, err)
	}
	endpoint := *c.base
	endpoint.Path, endpoint.RawPath = path, ""
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Attachment{}, errors.Join(ErrAttachmentClientInvalid, err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("X-Paperboat-Machine-Identity", identity)
	httpRequest.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Idempotency-Key", request.IdempotencyKey)
	httpRequest.Header.Set("If-Match", request.LeaseETag)
	httpRequest.Header.Set("Request-Id", request.RequestID)
	httpRequest.Header.Set("Correlation-Id", request.CorrelationID)
	response, err := c.client.Do(httpRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Attachment{}, err
		}
		return Attachment{}, errors.Join(ErrAttachmentClientUnavailable, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		return Attachment{}, errors.Join(ErrAttachmentClientUnavailable, err)
	}
	if int64(len(data)) > c.maxResponseBytes {
		return Attachment{}, fmt.Errorf("%w: response exceeds %d bytes", ErrAttachmentClientInvalid, c.maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusPreconditionFailed || response.StatusCode == http.StatusPreconditionRequired {
			return Attachment{}, errors.Join(ErrAttachmentLeaseETagStale, &AttachmentHTTPError{StatusCode: response.StatusCode})
		}
		retryable := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return Attachment{}, &AttachmentHTTPError{StatusCode: response.StatusCode, Retryable: retryable}
	}
	attachment, err := decodeAttachmentEnvelope(data, time.Now().UTC())
	if err != nil {
		return Attachment{}, err
	}
	if err := validateAttachmentForRequest(attachment, request); err != nil {
		return Attachment{}, err
	}
	return attachment, nil
}

// WaitForAdmission replays the same idempotent allocation until the server
// accepts the edge publication. It returns for admitted, edge_ready, or ready
// and never marks any state locally. A context deadline is the caller's bound
// for an edge that has not accepted the admission yet.
func (c *AttachmentClient) WaitForAdmission(ctx context.Context, request AttachmentRequest, initial Attachment) (Attachment, error) {
	if c == nil || ctx == nil {
		return Attachment{}, ErrAttachmentClientInvalid
	}
	if err := request.Validate(); err != nil {
		return Attachment{}, err
	}
	if strings.TrimSpace(request.LeaseETag) == "" {
		return Attachment{}, api.ErrPreviewLeaseETagRequired
	}
	if err := validateAttachmentForRequest(initial, request); err != nil {
		return Attachment{}, err
	}
	current := initial
	for {
		if current.State == "admitted" || (current.State == "edge_ready" || current.State == "ready") && current.EdgeReady {
			return current, nil
		}
		if current.State != "pending" {
			return Attachment{}, fmt.Errorf("%w: attachment cannot become admitted from %s", ErrAttachmentBinding, current.State)
		}
		timer := time.NewTimer(c.admissionPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Attachment{}, ctx.Err()
		case <-timer.C:
		}
		next, err := c.Allocate(ctx, request)
		if err != nil {
			return Attachment{}, err
		}
		current = next
	}
}

// WaitForEdgeReady replays the same idempotent allocation after the host has
// connected the admitted carrier. The server's edge observation is the only
// source of edge readiness; this method never promotes an admitted response
// locally.
func (c *AttachmentClient) WaitForEdgeReady(ctx context.Context, request AttachmentRequest, initial Attachment) (Attachment, error) {
	if c == nil || ctx == nil {
		return Attachment{}, ErrAttachmentClientInvalid
	}
	if err := request.Validate(); err != nil {
		return Attachment{}, err
	}
	if strings.TrimSpace(request.LeaseETag) == "" {
		return Attachment{}, api.ErrPreviewLeaseETagRequired
	}
	if err := validateAttachmentForRequest(initial, request); err != nil {
		return Attachment{}, err
	}
	current := initial
	for {
		if (current.State == "edge_ready" || current.State == "ready") && current.EdgeReady {
			return current, nil
		}
		if current.State != "pending" && current.State != "admitted" {
			return Attachment{}, fmt.Errorf("%w: attachment cannot become edge-ready from %s", ErrAttachmentBinding, current.State)
		}
		timer := time.NewTimer(c.admissionPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Attachment{}, ctx.Err()
		case <-timer.C:
		}
		next, err := c.Allocate(ctx, request)
		if err != nil {
			return Attachment{}, err
		}
		current = next
	}
}

// WaitForEdgeReadyCurrent follows the same attachment while lease renewals
// advance its strong ETag. A stale If-Match during a poll is retried with the
// next current lease; it never marks edge readiness locally.
func (c *AttachmentClient) WaitForEdgeReadyCurrent(ctx context.Context, initial Attachment, request func() (AttachmentRequest, error)) (Attachment, error) {
	if c == nil || ctx == nil || request == nil {
		return Attachment{}, ErrAttachmentClientInvalid
	}
	current := initial
	for {
		if (current.State == "edge_ready" || current.State == "ready") && current.EdgeReady {
			return current, nil
		}
		if current.State != "pending" && current.State != "admitted" {
			return Attachment{}, fmt.Errorf("%w: attachment cannot become edge-ready from %s", ErrAttachmentBinding, current.State)
		}
		timer := time.NewTimer(c.admissionPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Attachment{}, ctx.Err()
		case <-timer.C:
		}
		latestRequest, err := request()
		if err != nil {
			return Attachment{}, err
		}
		next, err := c.Allocate(ctx, latestRequest)
		if errors.Is(err, ErrAttachmentLeaseETagStale) {
			continue
		}
		if err != nil {
			return Attachment{}, err
		}
		current = next
	}
}

// ObserveOrigin records the result of the local origin probe through the
// machine-proof readiness endpoint. The response remains attachment state;
// the caller must separately invoke the preview lease readiness callback.
func (c *AttachmentClient) ObserveOrigin(ctx context.Context, request AttachmentRequest, attachment Attachment, originReady bool) (Attachment, error) {
	if c == nil || c.base == nil || ctx == nil {
		return Attachment{}, ErrAttachmentClientInvalid
	}
	if err := request.Validate(); err != nil {
		return Attachment{}, err
	}
	if strings.TrimSpace(request.LeaseETag) == "" {
		return Attachment{}, api.ErrPreviewLeaseETagRequired
	}
	if err := validateAttachmentForRequest(attachment, request); err != nil {
		return Attachment{}, err
	}
	if _, err := attachment.Admission(); err != nil {
		return Attachment{}, err
	}
	type mutation struct {
		Request              AttachmentRequest `json:"request"`
		Binding              Binding           `json:"binding"`
		AttachmentGeneration uint64            `json:"attachment_generation"`
		OriginReady          bool              `json:"origin_ready"`
	}
	body, err := json.Marshal(mutation{Request: request, Binding: attachment.Binding, AttachmentGeneration: attachment.AttachmentGeneration, OriginReady: originReady})
	if err != nil || len(body) == 0 || len(body) > attachmentRequestMaxBytes {
		return Attachment{}, fmt.Errorf("%w: readiness body is invalid", ErrAttachmentClientInvalid)
	}
	path := "/v1/previews/" + url.PathEscape(request.PreviewID) + "/carrier-attachment/readiness"
	tokenSource := c.tokens
	if tokenSource == nil {
		tokenSource = c.identities
	}
	token, err := tokenSource.Token(ctx)
	if err != nil || token == "" || len(token) > attachmentIdentityMaxBytes || hasAttachmentControl(token) {
		return Attachment{}, errors.Join(ErrAttachmentClientUnavailable, err)
	}
	identity, err := c.identities.Token(ctx)
	if err != nil || identity == "" || len(identity) > attachmentIdentityMaxBytes || hasAttachmentControl(identity) {
		return Attachment{}, errors.Join(ErrAttachmentClientUnavailable, err)
	}
	proof, err := c.proofs.Proof(ctx, request.OperationID, http.MethodPost, path, body)
	if err != nil || len(proof) == 0 || len(proof) > attachmentProofMaxBytes {
		return Attachment{}, errors.Join(ErrAttachmentClientUnavailable, err)
	}
	endpoint := *c.base
	endpoint.Path, endpoint.RawPath = path, ""
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Attachment{}, errors.Join(ErrAttachmentClientInvalid, err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("X-Paperboat-Machine-Identity", identity)
	httpRequest.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Idempotency-Key", request.IdempotencyKey)
	httpRequest.Header.Set("If-Match", request.LeaseETag)
	httpRequest.Header.Set("Request-Id", request.RequestID)
	httpRequest.Header.Set("Correlation-Id", request.CorrelationID)
	response, err := c.client.Do(httpRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Attachment{}, err
		}
		return Attachment{}, errors.Join(ErrAttachmentClientUnavailable, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		return Attachment{}, errors.Join(ErrAttachmentClientUnavailable, err)
	}
	if int64(len(data)) > c.maxResponseBytes {
		return Attachment{}, fmt.Errorf("%w: response exceeds %d bytes", ErrAttachmentClientInvalid, c.maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusPreconditionFailed || response.StatusCode == http.StatusPreconditionRequired {
			return Attachment{}, errors.Join(ErrAttachmentLeaseETagStale, &AttachmentHTTPError{StatusCode: response.StatusCode})
		}
		retryable := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return Attachment{}, &AttachmentHTTPError{StatusCode: response.StatusCode, Retryable: retryable}
	}
	next, err := decodeAttachmentEnvelope(data, time.Now().UTC())
	if err != nil {
		return Attachment{}, err
	}
	if err := validateAttachmentForRequest(next, request); err != nil {
		return Attachment{}, err
	}
	if next.Binding != attachment.Binding {
		return Attachment{}, fmt.Errorf("%w: readiness response binding changed", ErrAttachmentBinding)
	}
	// A failed origin probe is a durable, idempotent observation. The server
	// may already have recorded the same false result while this carrier was
	// reconnecting, so an unchanged edge_ready generation is a valid replay.
	// Successful readiness must still advance the generation before it can be
	// accepted by the host.
	if !originReady && next.AttachmentGeneration == attachment.AttachmentGeneration && next.State == "edge_ready" && next.EdgeReady && !next.OriginReady {
		return next, nil
	}
	if next.AttachmentGeneration <= attachment.AttachmentGeneration {
		return Attachment{}, fmt.Errorf("%w: readiness response did not advance the current attachment", ErrAttachmentBinding)
	}
	return next, nil
}
