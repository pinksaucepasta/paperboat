package preview

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	yamux "github.com/libp2p/go-yamux/v5"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"io"
)

var (
	ErrAttachmentCarrierInvalid     = errors.New("invalid preview attachment carrier")
	ErrAttachmentCarrierClosed      = errors.New("preview attachment carrier is closed")
	ErrAttachmentCarrierUnavailable = errors.New("preview attachment carrier is unavailable")
	ErrAttachmentPlacementChanged   = errors.New("preview attachment placement changed")
)

// AttachmentAllocator is the small request/response boundary used by the
// lazy carrier. AttachmentClient is the production implementation; the
// interface keeps lifecycle tests independent of HTTP while preserving the
// exact server-issued attachment contract.
type AttachmentAllocator interface {
	Allocate(context.Context, AttachmentRequest) (Attachment, error)
}

// AttachmentCarrierConfig composes the machine-proof attachment client with
// the route-scoped provider. Allocation is intentionally lazy because the
// create operation ID exists only after the server creates the preview lease.
type AttachmentCarrierConfig struct {
	Attachments AttachmentAllocator
	Provider    PreviewCarrierProvider

	RequestID             func() (string, error)
	CorrelationID         func() (string, error)
	CloseTimeout          time.Duration
	PlacementPollInterval time.Duration
}

// AttachmentCarrier allocates one short-lived server attachment on the first
// carrier attempt, then delegates streams to the provider's generation-fenced
// DataCarrierPreviewHub. The request is retained across retry attempts so an
// uncertain HTTP result replays the same operation/body hash rather than
// accidentally creating a conflicting attachment.
type AttachmentCarrier struct {
	attachments  AttachmentAllocator
	provider     PreviewCarrierProvider
	requestID    func() (string, error)
	correlation  func() (string, error)
	closeWait    time.Duration
	pollInterval time.Duration

	mu      sync.Mutex
	closed  bool
	request AttachmentRequest
	set     bool
	current Carrier
}

func NewAttachmentCarrier(config AttachmentCarrierConfig) (*AttachmentCarrier, error) {
	if config.Attachments == nil || config.Provider == nil {
		return nil, ErrAttachmentCarrierInvalid
	}
	if config.CloseTimeout == 0 {
		config.CloseTimeout = PreviewLeaseDefaultShutdown
	}
	if config.CloseTimeout <= 0 || config.CloseTimeout > time.Minute {
		return nil, ErrAttachmentCarrierInvalid
	}
	if config.PlacementPollInterval == 0 {
		config.PlacementPollInterval = 5 * time.Second
	}
	if config.PlacementPollInterval < 10*time.Millisecond || config.PlacementPollInterval > time.Minute {
		return nil, ErrAttachmentCarrierInvalid
	}
	if config.RequestID == nil {
		config.RequestID = func() (string, error) { return newAttachmentTraceID("request") }
	}
	if config.CorrelationID == nil {
		config.CorrelationID = func() (string, error) { return newAttachmentTraceID("correlation") }
	}
	return &AttachmentCarrier{
		attachments:  config.Attachments,
		provider:     config.Provider,
		requestID:    config.RequestID,
		correlation:  config.CorrelationID,
		closeWait:    config.CloseTimeout,
		pollInterval: config.PlacementPollInterval,
	}, nil
}

func (c *AttachmentCarrier) Run(ctx context.Context, lease Lease, ready func(Lease) error) error {
	return c.RunWithLease(ctx, func() Lease { return lease }, ready)
}

// RunWithLease observes the latest lease ETag while a ready preview is live.
// Session renewals may advance that ETag independently of carrier placement.
func (c *AttachmentCarrier) RunWithLease(ctx context.Context, currentLease func() Lease, ready func(Lease) error) error {
	if c == nil || ctx == nil || currentLease == nil || ready == nil {
		return ErrAttachmentCarrierInvalid
	}
	lease := currentLease()
	request, err := c.requestForLease(lease)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrAttachmentCarrierClosed
	}
	c.mu.Unlock()
	attachment, err := c.attachments.Allocate(ctx, request)
	if err != nil {
		logAttachmentFailure(ctx, "allocate", err)
		return classifyAttachmentCarrierError(err)
	}
	if waiter, ok := c.attachments.(AttachmentAdmissionWaiter); ok {
		attachment, err = waiter.WaitForAdmission(ctx, request, attachment)
		if err != nil {
			logAttachmentFailure(ctx, "admission", err)
			return classifyAttachmentCarrierError(err)
		}
	}
	if attachment.State == "pending" {
		return classifyAttachmentCarrierError(ErrAttachmentAdmissionPending)
	}
	if attachment.State != "admitted" && (attachment.State != "edge_ready" && attachment.State != "ready" || !attachment.EdgeReady) {
		return fmt.Errorf("%w: attachment is not edge-ready", ErrAttachmentBinding)
	}
	carrier, err := c.provider.CarrierForAttachment(ctx, lease, attachment)
	if err != nil {
		logAttachmentFailure(ctx, "acquire", err)
		// Edge admission and local origin readiness are separate state
		// transitions. If the edge has accepted this attachment but the
		// host cannot acquire the authenticated carrier, publish the negative
		// origin result before retrying. Otherwise the control plane remains
		// stuck at edge_ready with no evidence that the host attempted the
		// origin side of the handshake.
		if errors.Is(err, ErrPreviewCarrierProviderUnavailable) {
			err = c.reportOriginFailure(ctx, request, attachment, err)
		}
		return classifyAttachmentCarrierError(err)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.closeWait)
		defer cancel()
		_ = carrier.Close(closeCtx)
		return ErrAttachmentCarrierClosed
	}
	c.current = carrier
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.current == carrier {
			c.current = nil
		}
		c.mu.Unlock()
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.closeWait)
		defer cancel()
		_ = carrier.Close(closeCtx)
	}()
	// An admitted attachment is intentionally dialed before waiting for the
	// edge observation. The authenticated carrier connection is what allows
	// the edge to transition this attachment to edge_ready.
	if attachment.State == "admitted" {
		if waiter, ok := c.attachments.(interface {
			WaitForEdgeReadyCurrent(context.Context, Attachment, func() (AttachmentRequest, error)) (Attachment, error)
		}); ok {
			attachment, err = waiter.WaitForEdgeReadyCurrent(ctx, attachment, func() (AttachmentRequest, error) {
				return c.requestForLease(currentLease())
			})
		} else if waiter, ok := c.attachments.(AttachmentEdgeReadyWaiter); ok {
			attachment, err = waiter.WaitForEdgeReady(ctx, request, attachment)
		} else {
			return classifyAttachmentCarrierError(ErrAttachmentAdmissionPending)
		}
		if err != nil {
			logAttachmentFailure(ctx, "edge_ready", err)
			return classifyAttachmentCarrierError(err)
		}
	}
	if !attachment.EdgeReady || (attachment.State != "edge_ready" && attachment.State != "ready") {
		return classifyAttachmentCarrierError(ErrAttachmentAdmissionPending)
	}
	var observedAttachment = attachment
	carrierCtx, stopCarrier := context.WithCancel(ctx)
	defer stopCarrier()
	readyForPlacement := make(chan struct{})
	placementChanged := make(chan struct{}, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-readyForPlacement:
		case <-carrierCtx.Done():
			return
		}
		ticker := time.NewTicker(c.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-carrierCtx.Done():
				return
			case <-ticker.C:
			}
			currentRequest, err := c.requestForLease(currentLease())
			if err != nil {
				continue
			}
			latest, err := c.attachments.Allocate(carrierCtx, currentRequest)
			if err != nil {
				continue
			}
			if latest.Binding != attachment.Binding {
				placementChanged <- struct{}{}
				stopCarrier()
				return
			}
		}
	}()
	var readyOnce sync.Once
	err = carrier.Run(carrierCtx, lease, func(observed Lease) error {
		if observedAttachment.State != "ready" || !observedAttachment.OriginReady {
			observer, ok := c.attachments.(AttachmentReadinessObserver)
			if !ok {
				return classifyAttachmentCarrierError(fmt.Errorf("%w: attachment readiness observer is unavailable", ErrAttachmentClientUnavailable))
			}
			// DataCarrierPreviewCarrier invokes this callback only after its
			// bounded origin probe succeeds. The observed lease is a copy of the
			// pre-probe dispatch projection, so its old OriginState is not an
			// observation source.
			currentRequest, requestErr := c.requestForLease(currentLease())
			if requestErr != nil {
				return classifyAttachmentCarrierError(requestErr)
			}
			next, err := observer.ObserveOrigin(ctx, currentRequest, observedAttachment, true)
			if errors.Is(err, ErrAttachmentLeaseETagStale) {
				currentRequest, requestErr = c.requestForLease(currentLease())
				if requestErr == nil {
					next, err = observer.ObserveOrigin(ctx, currentRequest, observedAttachment, true)
				}
			}
			if err != nil {
				return classifyAttachmentCarrierError(err)
			}
			observedAttachment = next
		}
		if err := ready(observed); err != nil {
			return err
		}
		readyOnce.Do(func() { close(readyForPlacement) })
		return nil
	})
	stopCarrier()
	<-watchDone
	select {
	case <-placementChanged:
		if ctx.Err() == nil {
			return &RetryableCarrierError{Err: ErrAttachmentPlacementChanged}
		}
	default:
	}
	if err != nil && errors.Is(err, ErrDataCarrierPreviewOrigin) {
		err = c.reportOriginFailure(ctx, request, observedAttachment, err)
	}
	return classifyAttachmentCarrierError(err)
}

// reportOriginFailure records the host-side half of an edge-admitted
// attachment before the caller retries. The original failure remains the
// primary error: readiness reporting is observability and state convergence,
// not permission to hide a failed carrier attempt. A canceled attempt never
// issues a late mutation.
func (c *AttachmentCarrier) reportOriginFailure(ctx context.Context, request AttachmentRequest, attachment Attachment, cause error) error {
	if cause == nil || ctx == nil || ctx.Err() != nil {
		return cause
	}
	// Origin observations are valid only after the edge has observed the live
	// carrier. An admitted response cannot be used to publish a negative
	// origin result because that would skip the edge-ready transition.
	if !attachment.EdgeReady || attachment.State != "edge_ready" && attachment.State != "ready" {
		return cause
	}
	observer, ok := c.attachments.(AttachmentReadinessObserver)
	if !ok {
		return cause
	}
	if _, err := observer.ObserveOrigin(ctx, request, attachment, false); err != nil {
		return errors.Join(cause, classifyAttachmentCarrierError(err))
	}
	return cause
}

func (c *AttachmentCarrier) requestForLease(lease Lease) (AttachmentRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return AttachmentRequest{}, ErrAttachmentCarrierClosed
	}
	if c.set {
		if c.request.PreviewID != lease.ID || c.request.OperationID != lease.CreateOperationID || c.request.OwnerMachineID != lease.OwnerMachineID || c.request.OwnerSessionID != lease.OwnerSessionID {
			return AttachmentRequest{}, fmt.Errorf("%w: lease identity changed during retry", ErrAttachmentCarrierInvalid)
		}
		// Lease renewal advances only the strong ETag. The signed operation
		// body remains immutable, but subsequent If-Match headers must use
		// the latest lease generation.
		if strings.TrimSpace(lease.ETag) != "" && lease.ETag != c.request.LeaseETag {
			if err := api.ValidatePreviewLeaseETag(lease.ID, lease.ETag); err != nil {
				return AttachmentRequest{}, fmt.Errorf("%w: renewed lease ETag: %v", ErrAttachmentCarrierInvalid, err)
			}
			c.request.LeaseETag = strings.TrimSpace(lease.ETag)
		}
		return c.request, nil
	}
	requestID, err := c.requestID()
	if err != nil {
		return AttachmentRequest{}, errors.Join(ErrAttachmentCarrierUnavailable, err)
	}
	correlationID, err := c.correlation()
	if err != nil {
		return AttachmentRequest{}, errors.Join(ErrAttachmentCarrierUnavailable, err)
	}
	request, err := AttachmentRequestForLease(lease, strings.TrimSpace(requestID), strings.TrimSpace(correlationID))
	if err != nil {
		return AttachmentRequest{}, err
	}
	c.request = request
	c.set = true
	return request, nil
}

func classifyAttachmentCarrierError(err error) error {
	if err == nil {
		return nil
	}
	var httpErr *AttachmentHTTPError
	if errors.As(err, &httpErr) && httpErr.Retryable {
		return &RetryableCarrierError{Err: err}
	}
	if errors.Is(err, ErrAttachmentClientUnavailable) || errors.Is(err, ErrPreviewCarrierProviderUnavailable) {
		return &RetryableCarrierError{Err: err}
	}
	if errors.Is(err, ErrAttachmentAdmissionPending) {
		return &RetryableCarrierError{Err: err}
	}
	if errors.Is(err, ErrDataCarrierPreviewOrigin) {
		return &RetryableCarrierError{Err: err}
	}
	// Hostd owns renewal while carrier admission and origin probing are in
	// flight. A successful renewal can therefore make the readiness If-Match
	// stale. Retry the same attachment operation with Session.currentLease;
	// requestForLease updates only the transport ETag and keeps the immutable
	// operation body and endpoint identity.
	if errors.Is(err, ErrAttachmentLeaseETagStale) {
		return &RetryableCarrierError{Err: err}
	}
	return err
}

func (c *AttachmentCarrier) Close(ctx context.Context) error {
	if c == nil || ctx == nil {
		return ErrAttachmentCarrierInvalid
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	carrier := c.current
	c.current = nil
	c.mu.Unlock()
	if carrier == nil {
		return nil
	}
	return carrier.Close(ctx)
}

func newAttachmentTraceID(noun string) (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return noun + "_" + id.String(), nil
}

var _ Carrier = (*AttachmentCarrier)(nil)
var _ AttachmentAllocator = (*AttachmentClient)(nil)

// Global transport logs are discarded by the CLI; emit only fixed categories.
var attachmentDiagnosticLogger = slog.New(slog.NewTextHandler(os.Stderr, nil))

func logAttachmentFailure(ctx context.Context, phase string, err error) {
	code := previewDispatchFailureCode(err)
	var networkError net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline"
	case errors.Is(err, context.Canceled):
		code = "canceled"
	case errors.Is(err, connector.ErrDataCarrierAdmission):
		code = "carrier_admission"
	case errors.Is(err, connector.ErrDataCarrierClosed):
		code = "carrier_closed"
	case errors.Is(err, connector.ErrInvalidDataCarrierConfig):
		code = "carrier_config_invalid"
	case errors.Is(err, connector.ErrDataCarrierSessionSource):
		code = "carrier_source_invalid"
	case errors.Is(err, yamux.ErrSessionShutdown):
		code = "multiplexer_closed"
	case errors.Is(err, yamux.ErrInvalidVersion):
		code = "multiplexer_protocol_invalid"
	case errors.Is(err, io.EOF):
		code = "peer_closed"
	case errors.Is(err, connector.ErrDataCarrierUnavailable):
		code = "carrier_unavailable"
	case errors.As(err, &networkError) && networkError.Timeout():
		code = "timeout"
	}
	attributes := []any{"phase", phase, "code", code, "error_type", fmt.Sprintf("%T", err)}
	var dialError *connector.TransportDialError
	if errors.As(err, &dialError) {
		switch dialError.Transport {
		case connector.HTTP2, connector.HTTP3, connector.TCPMux, connector.QUIC:
			attributes = append(attributes, "transport", string(dialError.Transport))
		}
	}
	attachmentDiagnosticLogger.WarnContext(ctx, "preview attachment failed", attributes...)
}
