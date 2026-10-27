// Package previewv1 owns the control-plane policy for foreground preview
// leases. The public resource is deliberately smaller than the persistence
// row: generation is carried by the HTTP ETag and internal state dimensions
// are projected to the canonical preview-tunnel-v1 vocabulary.
package previewv1

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"github.com/google/uuid"

	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/naming"
	"github.com/pinksaucepasta/paperboat/server/internal/previewdomain"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

const (
	Schema            = "paperboat.preview-tunnel/v1"
	Kind              = "preview_lease"
	DefaultLease      = 30 * time.Minute
	DefaultMaxLease   = 24 * time.Hour
	DefaultOwnerGrace = 30 * time.Second
	// DefaultBackgroundOwnerGrace is how long a background preview (host-owned, with a user
	// deadline) survives without an owner heartbeat: the host runtime restarts for updates and
	// resumes its previews with the same URL.
	DefaultBackgroundOwnerGrace = 5 * time.Minute
	ReconcileBatchSize          = 100
	maxPreviewCursorPayload     = 1024
	maxPreviewCursorEncoded     = 2048
	maximumCreateTries          = 8
)

var (
	ErrInvalidInput       = previewtunnelstore.ErrInvalidInput
	ErrNotFound           = previewtunnelstore.ErrNotFound
	ErrOwnerDenied        = errors.New("preview lease ownership denied")
	ErrAttachmentNotReady = errors.New("preview carrier attachment is not ready")
	ErrDomainProjection   = errors.New("preview domain projection exceeds its limit")
)

// AttachmentReadiness is the server-authoritative gate between the carrier
// attachment lifecycle and the public preview lease. Implementations must
// require the exact operation, preview, owner machine, and foreground session
// to have both authenticated edge and origin readiness.
type AttachmentReadiness interface {
	RequirePreviewAttachmentReady(context.Context, string, string, string, string, string) error
}

// Repository is the narrow persistence boundary used by this service. All
// mutations remain transactional in previewtunnelstore, while policy tests
// can use a deterministic fake without PostgreSQL.
type Repository interface {
	ResolvePreviewManagementAccountV1(context.Context, string, string) (string, error)
	VerifyPreviewLeaseOwnerV1(context.Context, string, string, string) (bool, error)
	ResolvePreviewLeaseMachineAccountV1(context.Context, string, string, string) (string, error)
	GetPreviewLeaseV1(context.Context, string, string) (previewtunnelstore.PreviewLeaseRecord, error)
	GetPreviewLeaseCreateOperationV1(context.Context, string, string) (dbsqlc.Operation, error)
	ListPreviewLeasesV1(context.Context, previewtunnelstore.ListPreviewLeasesV1Input) ([]previewtunnelstore.PreviewLeaseRecord, error)
	CountPreviewLeasesV1(context.Context, string) (int, error)
	CreatePreviewLeaseV1(context.Context, previewtunnelstore.CreatePreviewLeaseV1Input) (previewtunnelstore.CreatePreviewLeaseV1Result, error)
	RenewPreviewLeaseV1(context.Context, previewtunnelstore.RenewPreviewLeaseV1Input) (previewtunnelstore.RenewPreviewLeaseV1Result, error)
	StopPreviewLeaseV1(context.Context, previewtunnelstore.StopPreviewLeaseV1Input) (previewtunnelstore.StopPreviewLeaseV1Result, error)
	MarkPreviewLeaseReadyV1(context.Context, previewtunnelstore.MarkPreviewLeaseReadyV1Input) (previewtunnelstore.PreviewLeaseRecord, error)
	ReconcilePreviewLeasesV1(context.Context, previewtunnelstore.ReconcilePreviewLeasesV1Input) (previewtunnelstore.ReconcilePreviewLeasesV1Result, error)
}

type Config struct {
	EndpointDomain       string
	CursorKey            []byte
	LeaseDuration        time.Duration
	MaxLease             time.Duration
	OwnerGrace           time.Duration
	BackgroundOwnerGrace time.Duration
	Dispatcher           Dispatcher
	AttachmentReadiness  AttachmentReadiness
	PreviewDomains       PreviewDomainReader
	Now                  func() time.Time
	NewID                func(prefix string) (string, error)
	Random               io.Reader
}

type Service struct {
	repository           Repository
	endpointDomain       string
	cursors              *cursorCodec
	leaseDuration        time.Duration
	maxLease             time.Duration
	ownerGrace           time.Duration
	backgroundOwnerGrace time.Duration
	dispatcher           Dispatcher
	attachmentReadiness  AttachmentReadiness
	previewDomains       PreviewDomainReader
	now                  func() time.Time
	newID                func(prefix string) (string, error)
	random               io.Reader
}

type PreviewDomainReader interface {
	List(context.Context, string, string, *previewdomain.ListPosition, int) ([]dbsqlc.PreviewDomain, error)
}

type previewDomainProjectionReader interface {
	ListProjections(context.Context, map[string]string) (map[string][]dbsqlc.PreviewDomain, error)
}

type Target struct {
	Scheme  string `json:"scheme"`
	Address string `json:"address"`
}

// CreateRequest is the server-facing form of POST /v1/previews. RequestHash
// is computed from the original JSON at the HTTP boundary so duplicate-field
// and idempotency semantics are preserved.
type CreateRequest struct {
	// ReservedEndpoint and Lazy are supplied only by authorized lazy activation.
	ReservedEndpoint string
	Lazy             *LazyBinding
	OwnerMachineID   string
	OwnerSessionID   string
	OwnerSessionKind string
	Target           Target
	AccessMode       string
	ExpiresAt        *time.Time
	Domains          []string
	IdempotencyKey   string
	RequestHash      [sha256.Size]byte
}

type MutationRequest struct {
	ExpectedGeneration int64
	// OwnerSessionID is the foreground session proof for renewal. Stop is
	// account-scoped and may be requested by a same-account browser.
	OwnerSessionID string
	IdempotencyKey string
	RequestHash    [sha256.Size]byte
}

type EndpointState string

const (
	EndpointReady                EndpointState = "ready"
	EndpointCustomDomainNotReady EndpointState = "custom_domain_not_ready"
	// EndpointEnded is a lease that ended (stopped, expired or its owner disconnected): it has no endpoint.
	EndpointEnded EndpointState = "ended"
)

type Preview struct {
	RouteID          string                 `json:"route_id,omitempty"`
	Schema           string                 `json:"schema"`
	Kind             string                 `json:"kind"`
	ID               string                 `json:"id"`
	AccountID        string                 `json:"account_id"`
	ActorID          string                 `json:"actor_id"`
	OwnerMachineID   string                 `json:"owner_machine_id"`
	OwnerSessionID   string                 `json:"owner_session_id"`
	OwnerSessionKind string                 `json:"owner_session_kind"`
	Target           Target                 `json:"target"`
	AccessMode       string                 `json:"access_mode"`
	Persistent       bool                   `json:"persistent"`
	Endpoint         string                 `json:"endpoint"`
	EndpointState    EndpointState          `json:"endpoint_state"`
	LeaseDeadline    time.Time              `json:"lease_deadline"`
	UserDeadline     *time.Time             `json:"user_deadline"`
	State            string                 `json:"state"`
	AllocationState  string                 `json:"allocation_state"`
	EdgeState        string                 `json:"edge_state"`
	OriginState      string                 `json:"origin_state"`
	CreatedAt        time.Time              `json:"created_at"`
	LastRenewedAt    time.Time              `json:"last_renewed_at"`
	Domains          []PreviewDomainSummary `json:"domains"`
}

// PreviewDomainSummary is the bounded alias projection embedded in a preview
// lease. Full account-scoped domain resources remain available from the
// nested domain endpoints and are not duplicated here.
type PreviewDomainSummary struct {
	ID             string                         `json:"id"`
	TargetKind     string                         `json:"target_kind"`
	PreviewID      string                         `json:"preview_id"`
	Hostname       string                         `json:"hostname"`
	MatchType      string                         `json:"match_type"`
	WildcardLabels *int                           `json:"wildcard_labels,omitempty"`
	State          string                         `json:"state"`
	DNS            previewdomain.DNSState         `json:"dns"`
	Certificate    previewdomain.CertificateState `json:"certificate"`
	Generation     int64                          `json:"generation"`
	ETag           string                         `json:"etag"`
}

type PreviewResult struct {
	Preview Preview
	ETag    string
}

type CreateResult struct {
	Preview   Preview
	Operation previewtunnelapi.Operation
	ETag      string
	Replayed  bool
}

type RenewResult struct {
	Preview   Preview
	Operation previewtunnelapi.Operation
	ETag      string
	Replayed  bool
}

type StopResult struct {
	Preview  Preview
	ETag     string
	Replayed bool
}

type PreviewPage struct {
	Items      []Preview `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
	Total      int       `json:"total"` // every preview the cursor pages through with these filters
}

type ReconcileResult struct {
	Expired   []Preview
	OwnerLost []Preview
	HasMore   bool
}

func NewService(repository Repository, config Config) (*Service, error) {
	if repository == nil {
		return nil, errors.New("preview lease repository is required")
	}
	domain, err := normalizeEndpointDomain(config.EndpointDomain)
	if err != nil {
		return nil, err
	}
	cursors, err := newCursorCodec(config.CursorKey)
	if err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	leaseDuration := config.LeaseDuration
	if leaseDuration == 0 {
		leaseDuration = DefaultLease
	}
	maxLease := config.MaxLease
	if maxLease == 0 {
		maxLease = DefaultMaxLease
	}
	ownerGrace := config.OwnerGrace
	if ownerGrace == 0 {
		ownerGrace = DefaultOwnerGrace
	}
	backgroundOwnerGrace := config.BackgroundOwnerGrace
	if backgroundOwnerGrace == 0 {
		backgroundOwnerGrace = DefaultBackgroundOwnerGrace
	}
	if backgroundOwnerGrace < ownerGrace {
		backgroundOwnerGrace = ownerGrace
	}
	if leaseDuration <= 0 || maxLease < leaseDuration || ownerGrace <= 0 {
		return nil, errors.New("preview lease durations must be positive and max lease must cover the default lease")
	}
	randomSource := config.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	idGenerator := config.NewID
	if idGenerator == nil {
		idGenerator = func(prefix string) (string, error) { return randomID(randomSource, prefix) }
	}
	return &Service{
		repository: repository, endpointDomain: domain, cursors: cursors,
		leaseDuration: leaseDuration, maxLease: maxLease, ownerGrace: ownerGrace, backgroundOwnerGrace: backgroundOwnerGrace,
		dispatcher: config.Dispatcher, attachmentReadiness: config.AttachmentReadiness, previewDomains: config.PreviewDomains,
		now: now, newID: idGenerator, random: randomSource,
	}, nil
}

func (s *Service) Create(ctx context.Context, request previewtunnelapi.RequestContext, input CreateRequest) (CreateResult, error) {
	if err := s.authorize(request, "write"); err != nil {
		return CreateResult{}, err
	}
	if (input.OwnerSessionKind == "lazy_runtime") != (input.Lazy != nil) {
		return CreateResult{}, ErrInvalidInput
	}
	if err := validateCreateRequest(input); err != nil {
		return CreateResult{}, err
	}
	domains, err := normalizePreviewDomains(input.Domains)
	if err != nil {
		return CreateResult{}, err
	}
	input.Domains = domains
	if err := ensureCreateOwner(request.Actor, input.OwnerMachineID, input.OwnerSessionID); err != nil {
		return CreateResult{}, err
	}
	binding := previewtunnelstore.LazyLeaseAuthority{AccountID: request.Actor.AccountID, MachineID: strings.TrimSpace(input.OwnerMachineID), OwnerSessionID: input.OwnerSessionID, OwnerSessionKind: input.OwnerSessionKind, TargetScheme: input.Target.Scheme, TargetAddress: input.Target.Address, Endpoint: input.ReservedEndpoint, ActivationID: input.IdempotencyKey}
	if input.Lazy != nil {
		binding.PolicyID = input.Lazy.PolicyID
		binding.PolicyGeneration = input.Lazy.PolicyGeneration
		binding.InstallationGeneration = input.Lazy.InstallationGeneration
	}
	shared, err := s.verifyOwner(ctx, binding, normalizedAccessMode(input.AccessMode))
	if err != nil {
		return CreateResult{}, err
	}
	now := s.now().UTC()
	accessMode := normalizedAccessMode(input.AccessMode)
	if accessMode != "public" && accessMode != "private" && accessMode != "team" {
		return CreateResult{}, fmt.Errorf("%w: access_mode must be public, private or team", ErrInvalidInput)
	}
	if !previewtunnelstore.ValidPreviewTargetV1(input.Target.Scheme, strings.TrimSpace(input.Target.Address), accessMode) {
		return CreateResult{}, fmt.Errorf("%w: target is not valid for its scheme and access mode", ErrInvalidInput)
	}
	leaseDeadline, userDeadline, err := s.deadlines(now, input.ExpiresAt)
	if shared && leaseDeadline.After(now.Add(5*time.Minute)) {
		leaseDeadline = now.Add(5 * time.Minute)
	}
	if err != nil {
		return CreateResult{}, err
	}
	var normalizeErr error
	request, normalizeErr = s.normalizeRequest(request)
	if normalizeErr != nil {
		return CreateResult{}, normalizeErr
	}
	requestHash := input.RequestHash
	if requestHash == ([sha256.Size]byte{}) {
		requestHash = hashCreateRequest(input)
	}
	for attempt := 0; attempt < maximumCreateTries; attempt++ {
		leaseID, err := s.newID("preview")
		if err != nil {
			return CreateResult{}, fmt.Errorf("allocate preview lease ID: %w", err)
		}
		endpoint, err := s.randomEndpoint()
		if input.ReservedEndpoint != "" {
			endpoint = input.ReservedEndpoint
		}
		if err != nil {
			return CreateResult{}, err
		}
		operationID, err := s.newID("operation")
		if err != nil {
			return CreateResult{}, fmt.Errorf("allocate preview operation identity: %w", err)
		}
		auditID, err := s.newID("event")
		if err != nil {
			return CreateResult{}, fmt.Errorf("allocate preview audit identity: %w", err)
		}
		result, err := s.repository.CreatePreviewLeaseV1(ctx, previewtunnelstore.CreatePreviewLeaseV1Input{
			OperationID: operationID, LeaseID: leaseID, AuditEventID: auditID,
			AccountID: request.Actor.AccountID, ActorID: request.Actor.ActorID, ActorType: actorType(request.Actor),
			OwnerMachineID: strings.TrimSpace(input.OwnerMachineID), OwnerSessionID: strings.TrimSpace(input.OwnerSessionID), OwnerSessionKind: input.OwnerSessionKind,
			TargetScheme: input.Target.Scheme, TargetAddress: strings.TrimSpace(input.Target.Address), AccessMode: accessMode,
			Endpoint: endpoint, LeaseDeadline: leaseDeadline, UserDeadline: userDeadline,
			RequestHash: requestHash[:], IdempotencyKey: input.IdempotencyKey, CorrelationID: request.CorrelationID,
			RequestID: request.RequestID, SourceMachineID: request.Actor.MachineID, Now: now,
			Domains: previewDomainCreateRequests(input.Domains),
		})
		if err == nil {
			view, viewErr := s.previewView(ctx, result.Lease)
			if viewErr != nil {
				return CreateResult{}, viewErr
			}
			createResult := CreateResult{Preview: view, Operation: operationView(result.Operation, request.RequestID),
				ETag: previewtunnelapi.ETag(Kind, result.Lease.ID, result.Lease.Generation), Replayed: result.Replayed}
			if s.dispatcher != nil && createOperationNeedsDispatch(createResult.Operation, result.Replayed) {
				if dispatchErr := s.dispatchCreatedLease(ctx, request, input.IdempotencyKey, createResult, input.Lazy, result.Lease.Endpoint); dispatchErr != nil {
					return CreateResult{}, dispatchErr
				}
			}
			return createResult, nil
		}
		if !isEndpointConflict(err) {
			return CreateResult{}, err
		}
	}
	return CreateResult{}, fmt.Errorf("%w: unable to allocate a unique preview endpoint", previewtunnelstore.ErrConflict)
}

// ConfigureDispatcher installs the post-commit host dispatcher. Keeping this
// separate from NewService lets app composition construct the signer and
// route resolver after the persistence service without changing policy tests.
func (s *Service) ConfigureDispatcher(dispatcher Dispatcher) {
	if s != nil {
		s.dispatcher = dispatcher
	}
}

func createOperationNeedsDispatch(operation previewtunnelapi.Operation, replayed bool) bool {
	switch operation.State {
	case "pending", "running", "uncertain":
		return true
	}
	// operationView intentionally exposes durable uncertainty as a failed
	// operation to clients, while retaining a typed retry signal. An exact
	// idempotent create replay must retry the same dispatch rather than leave
	// the lease pending forever after an uncertain network outcome.
	return replayed && operation.Error != nil && operation.Error.Retryable &&
		(operation.Error.Code == "operation_outcome_uncertain" || operation.Error.Code == "preview_dispatch_uncertain" || operation.Error.Outcome == "uncertain")
}

func (s *Service) dispatchCreatedLease(ctx context.Context, request previewtunnelapi.RequestContext, idempotencyKey string, created CreateResult, lazy *LazyBinding, identityEndpoint string) error {
	dispatch := DispatchRequest{
		Lazy:   lazy,
		Schema: Schema, Kind: PreviewDispatchKind, PreviewID: created.Preview.ID, OperationID: created.Operation.ID,
		AccountID: created.Preview.AccountID, ActorID: created.Preview.ActorID, OwnerMachineID: created.Preview.OwnerMachineID,
		OwnerSessionID: created.Preview.OwnerSessionID, OwnerSessionKind: created.Preview.OwnerSessionKind, Target: created.Preview.Target, AccessMode: created.Preview.AccessMode,
		Endpoint: identityEndpoint, LeaseDeadline: created.Preview.LeaseDeadline, UserDeadline: created.Preview.UserDeadline,
		LeaseETag: created.ETag, State: created.Preview.State, AllocationState: created.Preview.AllocationState,
		EdgeState: created.Preview.EdgeState, OriginState: created.Preview.OriginState, CreatedAt: created.Preview.CreatedAt,
		LastRenewedAt: created.Preview.LastRenewedAt, ExpectedGeneration: generationFromETag(created.ETag),
		IdempotencyKey: idempotencyKey, RequestID: request.RequestID, CorrelationID: request.CorrelationID,
	}
	hash, err := dispatch.ComputeRequestHash()
	if err != nil {
		return s.recordDispatchFailure(ctx, request, created, err, false)
	}
	dispatch.RequestHash = hash
	outcome, err := s.dispatcher.Dispatch(ctx, dispatch)
	if err != nil {
		return s.recordDispatchFailure(ctx, request, created, err, dispatchErrorUncertain(err))
	}
	if outcome.Schema != Schema || outcome.Kind != PreviewDispatchKind || outcome.PreviewID != dispatch.PreviewID || outcome.OperationID != dispatch.OperationID || (outcome.State != "accepted" && outcome.State != "ready" && outcome.State != "failed") || outcome.Generation < dispatch.ExpectedGeneration {
		return s.recordDispatchFailure(ctx, request, created, fmt.Errorf("%w: response binding mismatch", ErrDispatchMismatch), false)
	}
	if outcome.State == "failed" {
		return s.recordDispatchFailure(ctx, request, created, ErrDispatchFailed, false)
	}
	// accepted and ready are both transport acknowledgements. The host must
	// call the machine-auth readiness endpoint after the actual edge/origin
	// checks; only that CAS can complete the create operation.
	return nil
}

var (
	ErrDispatchFailed = errors.New("preview dispatch was rejected by the machine")
)

// A transport-owned outcome applies to its own unary cause chain. Joined
// siblings must each prove a known outcome; unresolved chains are uncertain so
// retries retain the committed operation identity instead of creating a second.
func dispatchErrorUncertain(err error) (uncertain bool) {
	uncertain = true
	defer func() {
		if recover() != nil {
			uncertain = true
		}
	}()
	remaining := 32
	var visit func(error) bool
	visit = func(current error) bool {
		if current == nil || remaining == 0 {
			return true
		}
		remaining--
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return true
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > remaining {
				return true
			}
			for _, child := range children {
				if visit(child) {
					return true
				}
			}
			return false
		}
		if classified, ok := current.(interface{ UncertainOutcome() bool }); ok {
			return classified.UncertainOutcome()
		}
		if current == ErrDispatchFailed || current == ErrDispatchRejected || current == ErrDispatchInvalid || current == ErrDispatchMismatch {
			return false
		}
		if wrapper, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapper.Unwrap())
		}
		return true
	}
	return visit(err)
}

func generationFromETag(etag string) int64 {
	parts := strings.Split(strings.Trim(etag, `"`), ":")
	if len(parts) != 4 {
		return 0
	}
	generation, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return 0
	}
	return generation
}

type dispatchUncertainRecorder interface {
	MarkPreviewLeaseDispatchUncertainV1(context.Context, previewtunnelstore.MarkPreviewLeaseDispatchUncertainV1Input) error
}

func (s *Service) recordDispatchFailure(ctx context.Context, request previewtunnelapi.RequestContext, created CreateResult, dispatchErr error, uncertain bool) error {
	if uncertain {
		if recorder, ok := s.repository.(dispatchUncertainRecorder); ok {
			auditID, idErr := s.newID("event")
			if idErr != nil {
				return errors.Join(dispatchErr, idErr)
			}
			if markErr := recorder.MarkPreviewLeaseDispatchUncertainV1(ctx, previewtunnelstore.MarkPreviewLeaseDispatchUncertainV1Input{
				AuditEventID: auditID, AccountID: created.Preview.AccountID, ActorID: created.Preview.ActorID, ActorType: actorType(request.Actor),
				PreviewID: created.Preview.ID, ErrorCode: "preview_dispatch_uncertain", CorrelationID: request.CorrelationID,
				RequestID: request.RequestID, SourceMachineID: request.Actor.MachineID, Now: s.now().UTC(),
			}); markErr != nil {
				return errors.Join(dispatchErr, markErr)
			}
		}
		// A repository that predates the optional uncertain-operation method
		// still receives the typed transport error. No ready state is reported.
		return dispatchErr
	}
	// Mark a known rejection as failed through the existing readiness CAS. It
	// advances the same generation and fails the original create operation in
	// one transaction, without fabricating readiness.
	auditID, err := s.newID("event")
	if err != nil {
		return errors.Join(dispatchErr, err)
	}
	_, markErr := s.repository.MarkPreviewLeaseReadyV1(ctx, previewtunnelstore.MarkPreviewLeaseReadyV1Input{
		AuditEventID: auditID, AccountID: created.Preview.AccountID, ActorID: created.Preview.ActorID, ActorType: actorType(request.Actor),
		// "unavailable" is the public preview projection. Persistence stores
		// the internal origin state "down" and previewView maps it back to
		// "unavailable" for clients.
		PreviewID: created.Preview.ID, ExpectedGeneration: generationFromETag(created.ETag), AllocationState: "failed", EdgeState: "down", OriginState: "down",
		CorrelationID: request.CorrelationID, RequestID: request.RequestID, SourceMachineID: request.Actor.MachineID, Now: s.now().UTC(),
	})
	if markErr != nil {
		return errors.Join(dispatchErr, markErr)
	}
	return dispatchErr
}

func (s *Service) Get(ctx context.Context, request previewtunnelapi.RequestContext, previewID string) (PreviewResult, error) {
	if err := s.authorize(request, "read"); err != nil {
		return PreviewResult{}, err
	}
	if strings.TrimSpace(previewID) == "" {
		return PreviewResult{}, fmt.Errorf("%w: preview ID is required", ErrInvalidInput)
	}
	request, accountErr := s.managementLeaseRequest(ctx, request, previewID)
	if accountErr != nil {
		return PreviewResult{}, accountErr
	}

	lease, err := s.repository.GetPreviewLeaseV1(ctx, request.Actor.AccountID, previewID)
	if err != nil {
		return PreviewResult{}, err
	}
	view, err := s.previewView(ctx, lease)
	if err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{Preview: view, ETag: previewtunnelapi.ETag(Kind, lease.ID, lease.Generation)}, nil
}

func (s *Service) List(ctx context.Context, request previewtunnelapi.RequestContext, rawCursor string, limit int) (PreviewPage, error) {
	if _, selected := workspace.FromContext(ctx); !selected {
		ctx = workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
	}
	if err := s.authorize(request, "read"); err != nil {
		return PreviewPage{}, err
	}
	if limit < 1 || limit > previewtunnelapi.MaximumPageLimit {
		return PreviewPage{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidInput, previewtunnelapi.MaximumPageLimit)
	}
	var after *previewCursor
	if strings.TrimSpace(rawCursor) != "" {
		position, err := s.cursors.Decode(rawCursor, workspaceCursorPrincipal(ctx, request.Actor.AccountID))
		if err != nil {
			return PreviewPage{}, err
		}
		after = &position
	}
	var afterCreatedAt sql.NullTime
	var afterID sql.NullString
	if after != nil {
		afterCreatedAt = sql.NullTime{Time: after.CreatedAt, Valid: true}
		afterID = sql.NullString{String: after.ID, Valid: true}
	}
	rows, err := s.repository.ListPreviewLeasesV1(ctx, previewtunnelstore.ListPreviewLeasesV1Input{
		AccountID: request.Actor.AccountID, AfterCreatedAt: afterCreatedAt, AfterID: afterID, Limit: int32(limit + 1),
	})
	if err != nil {
		return PreviewPage{}, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	total, err := s.repository.CountPreviewLeasesV1(ctx, request.Actor.AccountID)
	if err != nil {
		return PreviewPage{}, err
	}
	items, err := s.previewViews(ctx, rows)
	if err != nil {
		return PreviewPage{}, err
	}
	page := PreviewPage{Items: items, Total: total}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor, err = s.cursors.Encode(previewCursor{AccountID: workspaceCursorPrincipal(ctx, request.Actor.AccountID), CreatedAt: last.CreatedAt.UTC(), ID: last.ID})
		if err != nil {
			return PreviewPage{}, err
		}
	}
	return page, nil
}

func (s *Service) Renew(ctx context.Context, request previewtunnelapi.RequestContext, previewID string, mutation MutationRequest) (RenewResult, error) {
	if err := s.authorize(request, "write"); err != nil {
		return RenewResult{}, err
	}
	if err := validateMutationRequest(mutation); err != nil {
		return RenewResult{}, err
	}
	if strings.TrimSpace(mutation.OwnerSessionID) == "" {
		return RenewResult{}, fmt.Errorf("%w: owner session is required for renewal", ErrInvalidInput)
	}
	var normalizeErr error
	request, normalizeErr = s.normalizeRequest(request)
	if normalizeErr != nil {
		return RenewResult{}, normalizeErr
	}
	request, accountErr := s.machineLeaseRequest(ctx, request, previewID)
	if accountErr != nil {
		return RenewResult{}, accountErr
	}

	current, err := s.repository.GetPreviewLeaseV1(ctx, request.Actor.AccountID, previewID)
	if err != nil {
		return RenewResult{}, err
	}
	if err := ensureRenewCanManage(request, current, mutation.OwnerSessionID); err != nil {
		return RenewResult{}, err
	}
	shared, authorityErr := s.verifyOwner(ctx, lazyLeaseRecordAuthority(current), current.AccessMode)
	if authorityErr != nil {
		return RenewResult{}, authorityErr
	}
	now := s.now().UTC()
	newDeadline := now.Add(s.leaseDuration)
	if shared && newDeadline.After(now.Add(5*time.Minute)) {
		newDeadline = now.Add(5 * time.Minute)
	}
	if current.UserDeadline.Valid && newDeadline.After(current.UserDeadline.Time) {
		newDeadline = current.UserDeadline.Time
	}
	if !newDeadline.After(now) {
		return RenewResult{}, previewtunnelstore.ErrPreviewLeaseDeadlineExceeded
	}
	requestHash := mutation.RequestHash
	if requestHash == ([sha256.Size]byte{}) {
		requestHash = hashMutationRequest(previewID, mutation.ExpectedGeneration, &newDeadline)
	}
	operationID, err := s.newID("operation")
	if err != nil {
		return RenewResult{}, err
	}
	auditID, err := s.newID("event")
	if err != nil {
		return RenewResult{}, err
	}
	result, err := s.repository.RenewPreviewLeaseV1(ctx, previewtunnelstore.RenewPreviewLeaseV1Input{
		OperationID: operationID, AuditEventID: auditID, AccountID: request.Actor.AccountID, ActorID: request.Actor.ActorID,
		ActorType: actorType(request.Actor), PreviewID: previewID, ExpectedGeneration: mutation.ExpectedGeneration,
		OwnerMachineID: current.OwnerMachineID, OwnerSessionID: strings.TrimSpace(mutation.OwnerSessionID),
		LeaseDeadline: newDeadline, RequestHash: requestHash[:], IdempotencyKey: mutation.IdempotencyKey,
		CorrelationID: request.CorrelationID, RequestID: request.RequestID, SourceMachineID: request.Actor.MachineID, Now: now,
	})
	if err != nil {
		return RenewResult{}, err
	}
	view, err := s.previewView(ctx, result.Lease)
	if err != nil {
		return RenewResult{}, err
	}
	return RenewResult{Preview: view, Operation: operationView(result.Operation, request.RequestID),
		ETag: previewtunnelapi.ETag(Kind, result.Lease.ID, result.Lease.Generation), Replayed: result.Replayed}, nil
}

func (s *Service) Stop(ctx context.Context, request previewtunnelapi.RequestContext, previewID string, mutation MutationRequest) (StopResult, error) {
	if err := s.authorize(request, "write"); err != nil {
		return StopResult{}, err
	}
	if err := validateMutationRequest(mutation); err != nil {
		return StopResult{}, err
	}
	var normalizeErr error
	request, normalizeErr = s.normalizeRequest(request)
	if normalizeErr != nil {
		return StopResult{}, normalizeErr
	}
	managementAccount := request.Actor.AccountID
	if request.Actor.HostID != "" {
		managementAccount = ""
	}
	request, accountErr := s.managementLeaseRequest(ctx, request, previewID)
	if accountErr != nil {
		return StopResult{}, accountErr
	}

	current, err := s.repository.GetPreviewLeaseV1(ctx, request.Actor.AccountID, previewID)
	if err != nil {
		return StopResult{}, err
	}
	if managementAccount == "" {
		if err := ensureStopCanManage(request, current); err != nil {
			return StopResult{}, err
		}
	}
	operationID, err := s.newID("operation")
	if err != nil {
		return StopResult{}, err
	}
	auditID, err := s.newID("event")
	if err != nil {
		return StopResult{}, err
	}
	requestHash := mutation.RequestHash
	if requestHash == ([sha256.Size]byte{}) {
		requestHash = hashMutationRequest(previewID, mutation.ExpectedGeneration, nil)
	}
	result, err := s.repository.StopPreviewLeaseV1(ctx, previewtunnelstore.StopPreviewLeaseV1Input{
		ManagementAccountID: managementAccount,
		OperationID:         operationID, AuditEventID: auditID, AccountID: request.Actor.AccountID, ActorID: request.Actor.ActorID, ActorType: actorType(request.Actor),
		PreviewID: previewID, ExpectedGeneration: mutation.ExpectedGeneration, IdempotencyKey: mutation.IdempotencyKey,
		OwnerMachineID: current.OwnerMachineID, OwnerSessionID: current.OwnerSessionID, RequestHash: requestHash[:],
		CorrelationID: request.CorrelationID, RequestID: request.RequestID, SourceMachineID: request.Actor.MachineID, Now: s.now().UTC(),
	})
	if err != nil {
		return StopResult{}, err
	}
	view, err := s.previewView(ctx, result.Lease)
	if err != nil {
		return StopResult{}, err
	}
	return StopResult{Preview: view, ETag: previewtunnelapi.ETag(Kind, result.Lease.ID, result.Lease.Generation), Replayed: result.Replayed}, nil
}

// Reconcile expires leases and releases owners that missed the reconnect
// grace period. The worker supplies a system actor and this method performs no
// user authorization because it is not an HTTP endpoint.
func (s *Service) Reconcile(ctx context.Context, actorID, correlationID, requestID string) (ReconcileResult, error) {
	if strings.TrimSpace(actorID) == "" || strings.TrimSpace(correlationID) == "" || strings.TrimSpace(requestID) == "" {
		return ReconcileResult{}, fmt.Errorf("%w: reconciliation actor, request ID, and correlation ID are required", ErrInvalidInput)
	}
	now := s.now().UTC()
	result, err := s.repository.ReconcilePreviewLeasesV1(ctx, previewtunnelstore.ReconcilePreviewLeasesV1Input{
		ActorID: actorID, ActorType: "system", CorrelationID: correlationID, RequestID: requestID,
		Now: now, OwnerGrace: s.ownerGrace, BackgroundOwnerGrace: s.backgroundOwnerGrace, Limit: ReconcileBatchSize,
	})
	if err != nil {
		return ReconcileResult{}, err
	}
	expired, err := s.previewViews(ctx, result.Expired)
	if err != nil {
		return ReconcileResult{}, err
	}
	ownerLost, err := s.previewViews(ctx, result.OwnerLost)
	if err != nil {
		return ReconcileResult{}, err
	}
	return ReconcileResult{Expired: expired, OwnerLost: ownerLost, HasMore: result.HasMore}, nil
}

// ObserveReadiness applies a connector/edge observation with the same
// generation CAS used by renew and stop. Once all three dimensions are ready,
// the persistence layer also completes the original create operation.
func (s *Service) ObserveReadiness(ctx context.Context, request previewtunnelapi.RequestContext, previewID string, expectedGeneration int64, allocationState, edgeState, originState string) (PreviewResult, error) {
	if strings.TrimSpace(request.Actor.AccountID) == "" || strings.TrimSpace(request.Actor.ActorID) == "" {
		return PreviewResult{}, previewtunnelapi.ErrForbidden
	}
	if request.Actor.Role != "system_worker" && request.Actor.Role != "admin" && request.Actor.Role != "edge" {
		if err := s.authorize(request, "write"); err != nil {
			return PreviewResult{}, err
		}
	}
	request, err := s.normalizeRequest(request)
	if err != nil {
		return PreviewResult{}, err
	}
	auditID, err := s.newID("event")
	if err != nil {
		return PreviewResult{}, err
	}
	row, err := s.repository.MarkPreviewLeaseReadyV1(ctx, previewtunnelstore.MarkPreviewLeaseReadyV1Input{
		AuditEventID: auditID, AccountID: request.Actor.AccountID, ActorID: request.Actor.ActorID,
		ActorType: actorType(request.Actor), PreviewID: previewID, ExpectedGeneration: expectedGeneration,
		AllocationState: allocationState, EdgeState: edgeState, OriginState: originState,
		CorrelationID: request.CorrelationID, RequestID: request.RequestID, SourceMachineID: request.Actor.MachineID,
		Now: s.now().UTC(),
	})
	if err != nil {
		return PreviewResult{}, err
	}
	view, err := s.previewView(ctx, row)
	if err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{Preview: view, ETag: previewtunnelapi.ETag(Kind, row.ID, row.Generation)}, nil
}

// ObserveMachineReadiness is the only external readiness mutation for a
// dashboard-dispatched preview. The caller must be a machine-authenticated
// actor for the exact owner machine and foreground session, and must present
// the current strong lease ETag. It intentionally reads the lease before the
// transactional readiness CAS so owner identity checks cannot be supplied by
// an untrusted request body alone.
func (s *Service) ObserveMachineReadiness(ctx context.Context, request previewtunnelapi.RequestContext, previewID, operationID, ownerMachineID, ownerSessionID, leaseETag string, expectedGeneration int64, allocationState, edgeState, originState string) (PreviewResult, error) {
	if strings.TrimSpace(request.Actor.AccountID) == "" || strings.TrimSpace(request.Actor.ActorID) == "" || (strings.TrimSpace(request.Actor.MachineID) == "" && strings.TrimSpace(request.Actor.HostID) == "") {
		return PreviewResult{}, previewtunnelapi.ErrForbidden
	}
	if strings.TrimSpace(ownerMachineID) == "" || strings.TrimSpace(ownerSessionID) == "" || strings.TrimSpace(previewID) == "" || strings.TrimSpace(operationID) == "" || expectedGeneration < 1 {
		return PreviewResult{}, fmt.Errorf("%w: readiness identity and generation are required", ErrInvalidInput)
	}
	// The host reports its local app going down and coming back; allocation and edge stay ready.
	if allocationState != "ready" || edgeState != "ready" || (originState != "ready" && originState != "down") {
		return PreviewResult{}, fmt.Errorf("%w: readiness requires ready allocation and edge and an origin that is ready or down", ErrInvalidInput)
	}
	ownerMachineID = strings.TrimSpace(ownerMachineID)
	ownerSessionID = strings.TrimSpace(ownerSessionID)
	if request.Actor.MachineID != ownerMachineID && request.Actor.HostID != ownerMachineID {
		return PreviewResult{}, ErrOwnerDenied
	}
	expectedETag := previewtunnelapi.ETag(Kind, previewID, expectedGeneration)
	if strings.TrimSpace(leaseETag) != expectedETag {
		return PreviewResult{}, previewtunnelapi.ErrInvalidETag
	}
	if err := s.authorize(request, "write"); err != nil {
		return PreviewResult{}, err
	}
	request, err := s.normalizeRequest(request)
	if err != nil {
		return PreviewResult{}, err
	}
	request, accountErr := s.machineLeaseRequest(ctx, request, previewID)
	if accountErr != nil {
		return PreviewResult{}, accountErr
	}

	current, err := s.repository.GetPreviewLeaseV1(ctx, request.Actor.AccountID, previewID)
	if err != nil {
		return PreviewResult{}, err
	}
	if _, authorityErr := s.verifyOwner(ctx, lazyLeaseRecordAuthority(current), current.AccessMode); authorityErr != nil {
		return PreviewResult{}, authorityErr
	}
	operation, err := s.repository.GetPreviewLeaseCreateOperationV1(ctx, request.Actor.AccountID, previewID)
	if err != nil {
		return PreviewResult{}, err
	}
	if operation.ID != strings.TrimSpace(operationID) || operation.OperationType != "preview.create" || operation.ResourceKind != "preview_lease" || !operation.ResourceID.Valid || operation.ResourceID.String != previewID {
		return PreviewResult{}, ErrOwnerDenied
	}
	if current.OwnerMachineID != ownerMachineID || current.OwnerSessionID != ownerSessionID {
		return PreviewResult{}, ErrOwnerDenied
	}
	if s.attachmentReadiness == nil {
		return PreviewResult{}, ErrAttachmentNotReady
	}
	if err := s.attachmentReadiness.RequirePreviewAttachmentReady(ctx, request.Actor.AccountID, previewID, operationID, ownerMachineID, ownerSessionID); err != nil {
		return PreviewResult{}, errors.Join(ErrAttachmentNotReady, err)
	}
	if current.TerminalState != "active" {
		return PreviewResult{}, previewtunnelstore.ErrPreviewLeaseTerminal
	}
	if current.Generation != expectedGeneration {
		// A host may not know whether its previous readiness response reached the
		// server. An exact replay after the CAS is therefore successful when the
		// current projection is already the requested ready state. A different
		// owner, session, or lifecycle body remains a normal generation conflict.
		if current.TerminalState == "active" && current.AllocationState == allocationState && current.EdgeState == edgeState && current.OriginState == originState {
			view, viewErr := s.previewView(ctx, current)
			if viewErr != nil {
				return PreviewResult{}, viewErr
			}
			return PreviewResult{Preview: view, ETag: previewtunnelapi.ETag(Kind, current.ID, current.Generation)}, nil
		}
		return PreviewResult{}, previewtunnelstore.ErrGenerationConflict
	}
	auditID, err := s.newID("event")
	if err != nil {
		return PreviewResult{}, err
	}
	row, err := s.repository.MarkPreviewLeaseReadyV1(ctx, previewtunnelstore.MarkPreviewLeaseReadyV1Input{
		AuditEventID: auditID, AccountID: request.Actor.AccountID, ActorID: request.Actor.ActorID,
		ActorType: actorType(request.Actor), PreviewID: previewID, ExpectedGeneration: expectedGeneration,
		AllocationState: allocationState, EdgeState: edgeState, OriginState: originState,
		CorrelationID: request.CorrelationID, RequestID: request.RequestID, SourceMachineID: ownerMachineID,
		Now: s.now().UTC(),
	})
	if err != nil {
		return PreviewResult{}, err
	}
	view, err := s.previewView(ctx, row)
	if err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{Preview: view, ETag: previewtunnelapi.ETag(Kind, row.ID, row.Generation)}, nil
}

func (s *Service) previewView(ctx context.Context, row previewtunnelstore.PreviewLeaseRecord) (Preview, error) {
	views, err := s.previewViews(ctx, []previewtunnelstore.PreviewLeaseRecord{row})
	if err != nil {
		return Preview{}, err
	}
	return views[0], nil
}

// previewViews describes a page of previews with one query for their routes and one for their domains (a list
// ran two queries per preview, 104 for a 50-preview page).
func (s *Service) previewViews(ctx context.Context, rows []previewtunnelstore.PreviewLeaseRecord) ([]Preview, error) {
	previews := make(map[string]string, len(rows))
	for _, row := range rows {
		previews[row.ID] = row.AccountID
	}
	routes := map[string]string{}
	if reader, ok := s.repository.(interface {
		CurrentPreviewRoutes(context.Context, map[string]string) (map[string]string, error)
	}); ok {
		var err error
		if routes, err = reader.CurrentPreviewRoutes(ctx, previews); err != nil {
			return nil, err
		}
	}
	var domains map[string][]dbsqlc.PreviewDomain
	if projection, ok := s.previewDomains.(previewDomainProjectionReader); ok {
		var err error
		if domains, err = projection.ListProjections(ctx, previews); err != nil {
			return nil, err
		}
	}
	out := make([]Preview, 0, len(rows))
	for _, row := range rows {
		view := previewView(row)
		view.RouteID = routes[row.ID]
		if s.previewDomains == nil {
			out = append(out, view)
			continue
		}
		found, projected := domains[row.ID], domains != nil
		if !projected {
			var err error
			if found, err = s.previewDomains.List(ctx, row.AccountID, row.ID, nil, previewdomain.MaxDomains+1); err != nil {
				return nil, err
			}
		}
		if len(found) > previewdomain.MaxDomains {
			return nil, ErrDomainProjection
		}
		if len(found) > 0 {
			view.Domains = make([]PreviewDomainSummary, 0, len(found))
			for _, row := range found {
				domain := previewdomain.DomainViewFromRow(row)
				view.Domains = append(view.Domains, PreviewDomainSummary{
					ID: domain.ID, TargetKind: domain.TargetKind, PreviewID: domain.PreviewID,
					Hostname: domain.Hostname, MatchType: domain.MatchType, WildcardLabels: domain.WildcardLabels,
					State: domain.State, DNS: domain.DNS, Certificate: domain.Certificate,
					Generation: domain.Generation, ETag: domain.ETag,
				})
			}
		}
		out = append(out, view)
	}
	return out, nil
}

func (s *Service) authorize(request previewtunnelapi.RequestContext, action string) error {
	return previewtunnelapi.Authorize(request.Actor, previewtunnelapi.AccessRequest{
		AccountID: request.Actor.AccountID, Resource: "previews", Action: action,
	})
}

func (s *Service) normalizeRequest(request previewtunnelapi.RequestContext) (previewtunnelapi.RequestContext, error) {
	if strings.TrimSpace(request.RequestID) == "" {
		if value, err := s.newID("request"); err == nil {
			request.RequestID = value
		} else {
			return previewtunnelapi.RequestContext{}, fmt.Errorf("allocate preview request ID: %w", err)
		}
	}
	if strings.TrimSpace(request.CorrelationID) == "" {
		if value, err := s.newID("correlation"); err == nil {
			request.CorrelationID = value
		} else {
			return previewtunnelapi.RequestContext{}, fmt.Errorf("allocate preview correlation ID: %w", err)
		}
	}
	return request, nil
}

func (s *Service) deadlines(now time.Time, requested *time.Time) (time.Time, sql.NullTime, error) {
	if requested == nil {
		return now.Add(s.leaseDuration), sql.NullTime{}, nil
	}
	deadline := requested.UTC()
	if !deadline.After(now) {
		return time.Time{}, sql.NullTime{}, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidInput)
	}
	if deadline.Sub(now) > s.maxLease {
		return time.Time{}, sql.NullTime{}, fmt.Errorf("%w: expires_at exceeds the maximum preview lifetime", ErrInvalidInput)
	}
	return deadline, sql.NullTime{Time: deadline, Valid: true}, nil
}

func (s *Service) randomEndpoint() (string, error) {
	label, err := naming.PublicSlug(s.random)
	if err != nil {
		return "", err
	}
	return "https://" + label + "." + s.endpointDomain, nil
}

func previewView(row previewtunnelstore.PreviewLeaseRecord) Preview {
	endpoint := row.Endpoint
	mode := row.EdgeMode
	if mode == "" {
		mode = previewtunnelstore.EdgeModeManaged
	}
	if row.PublicEndpoint != nil {
		endpoint = *row.PublicEndpoint
	}
	edgeState := row.EdgeState
	if row.ManagedRoutePending && edgeState == "ready" {
		edgeState = "pending"
	}
	if edgeState == "released" {
		edgeState = "down"
	}
	originState := row.OriginState
	if originState == "degraded" || originState == "down" {
		originState = "unavailable"
	}
	endpointState := EndpointReady
	if mode == previewtunnelstore.EdgeModeSelfhost && endpoint == "" {
		endpointState = EndpointCustomDomainNotReady
	}
	allocationState := row.AllocationState
	if mode == previewtunnelstore.EdgeModeSelfhost && endpoint == "" {
		allocationState = "pending"
	}
	state := "connecting"
	switch row.TerminalState {
	case "owner_lost":
		state = "owner_disconnected"
	case "expired":
		state = "expired"
	case "stopped", "failed":
		state = "stopped"
	default:
		switch {
		case allocationState != "ready":
			state = "allocating"
		case edgeState == "ready" && originState == "unavailable":
			// The preview is attached and its edge works, but the local app is not running.
			state = "origin_unavailable"
		case edgeState != "ready" || originState != "ready":
			state = "connecting"
		default:
			state = "ready"
		}
	}
	if state == "owner_disconnected" || state == "expired" || state == "stopped" {
		endpoint, endpointState = "", EndpointEnded
	}
	view := Preview{
		Schema: Schema, Kind: Kind, ID: row.ID, AccountID: row.AccountID, ActorID: row.ActorID,
		OwnerMachineID: row.OwnerMachineID, OwnerSessionID: row.OwnerSessionID, OwnerSessionKind: row.OwnerSessionKind,
		Target: Target{Scheme: row.TargetScheme, Address: row.TargetAddress}, AccessMode: row.AccessMode,
		Persistent: false, Endpoint: endpoint, EndpointState: endpointState, LeaseDeadline: row.LeaseDeadline.UTC(), UserDeadline: nil,
		State: state, AllocationState: allocationState, EdgeState: edgeState, OriginState: originState,
		CreatedAt: row.CreatedAt.UTC(), LastRenewedAt: row.LastRenewedAt.UTC(), Domains: make([]PreviewDomainSummary, 0),
	}
	if row.UserDeadline.Valid {
		deadline := row.UserDeadline.Time.UTC()
		view.UserDeadline = &deadline
	}
	return view
}

func operationView(row dbsqlc.Operation, requestID string) previewtunnelapi.Operation {
	view := previewtunnelapi.Operation{
		Schema: Schema, Kind: "operation", ID: row.ID, ResourceKind: row.ResourceKind,
		ResourceID: nullableString(row.ResourceID), Phase: row.Phase, State: row.State,
		Progress: int(row.Progress), Retrying: row.Retrying, CorrelationID: row.CorrelationID,
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
	if row.State == "cancelled" {
		view.State = "canceled"
	}
	if row.State == "uncertain" {
		view.State = "failed"
		if !row.ErrorCode.Valid {
			row.ErrorCode = sql.NullString{String: "operation_outcome_uncertain", Valid: true}
		}
	}
	if row.NextRetryAt.Valid {
		next := row.NextRetryAt.Time.UTC()
		view.NextRetryAt = &next
	}
	if row.ErrorCode.Valid {
		view.Error = &previewtunnelapi.APIError{
			Schema: Schema, Kind: "error", Code: row.ErrorCode.String, Component: "control",
			Message: "The preview operation did not complete.", Outcome: row.Outcome, Retryable: row.Retrying,
			RepairAction: "inspect_operation", RequestID: requestID, CorrelationID: row.CorrelationID,
		}
	}
	return view
}

func validateCreateRequest(input CreateRequest) error {
	if strings.TrimSpace(input.OwnerMachineID) == "" || strings.TrimSpace(input.OwnerSessionID) == "" || !previewtunnelstore.ValidPreviewOwnerSessionKind(input.OwnerSessionKind) {
		return fmt.Errorf("%w: owner machine and session are required", ErrInvalidInput)
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" || len(input.IdempotencyKey) > 256 {
		return fmt.Errorf("%w: idempotency key is required", ErrInvalidInput)
	}
	if !previewtunnelstore.ValidPreviewTargetV1(input.Target.Scheme, strings.TrimSpace(input.Target.Address), normalizedAccessMode(input.AccessMode)) {
		return fmt.Errorf("%w: target is not valid for its scheme and access mode", ErrInvalidInput)
	}
	if _, err := normalizePreviewDomains(input.Domains); err != nil {
		return err
	}
	return nil
}

func normalizePreviewDomains(values []string) ([]string, error) {
	if len(values) > previewdomain.MaxDomains {
		return nil, fmt.Errorf("%w: at most %d preview domains are allowed", ErrInvalidInput, previewdomain.MaxDomains)
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		hostname, _, err := previewdomain.NormalizeHostname(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: preview domain is invalid", ErrInvalidInput)
		}
		if _, duplicate := seen[hostname]; duplicate {
			return nil, fmt.Errorf("%w: duplicate preview domain", ErrInvalidInput)
		}
		seen[hostname] = struct{}{}
		result = append(result, hostname)
	}
	sort.Strings(result)
	return result, nil
}

func previewDomainCreateRequests(hostnames []string) []previewtunnelstore.PreviewDomainCreateRequest {
	requests := make([]previewtunnelstore.PreviewDomainCreateRequest, len(hostnames))
	for index, hostname := range hostnames {
		requests[index] = previewtunnelstore.PreviewDomainCreateRequest{Hostname: hostname, Provider: "generic", CertificateStrategy: "managed"}
	}
	return requests
}

func normalizedAccessMode(raw string) string {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		return "public"
	}
	return mode
}

func validateMutationRequest(input MutationRequest) error {
	if input.ExpectedGeneration < 1 {
		return fmt.Errorf("%w: If-Match generation is required", ErrInvalidInput)
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" || len(input.IdempotencyKey) > 256 {
		return fmt.Errorf("%w: idempotency key is required", ErrInvalidInput)
	}
	return nil
}

func ensureRenewCanManage(request previewtunnelapi.RequestContext, row previewtunnelstore.PreviewLeaseRecord, ownerSessionID string) error {
	actor := request.Actor
	if actor.Role == "admin" || actor.Role == "system_worker" {
		return nil
	}
	if strings.TrimSpace(actor.MachineID) == "" && strings.TrimSpace(actor.HostID) == "" {
		return ErrOwnerDenied
	}
	if actor.MachineID != "" && actor.MachineID != row.OwnerMachineID {
		return ErrOwnerDenied
	}
	if actor.HostID != "" && actor.HostID != row.OwnerMachineID {
		return ErrOwnerDenied
	}
	if strings.TrimSpace(ownerSessionID) == "" || ownerSessionID != row.OwnerSessionID {
		return ErrOwnerDenied
	}
	return nil
}

func ensureStopCanManage(request previewtunnelapi.RequestContext, row previewtunnelstore.PreviewLeaseRecord) error {
	actor := request.Actor
	if actor.Role == "admin" || actor.Role == "system_worker" {
		return nil
	}
	if actor.MachineID != "" && actor.MachineID != row.OwnerMachineID {
		return ErrOwnerDenied
	}
	if actor.HostID != "" && actor.HostID != row.OwnerMachineID {
		return ErrOwnerDenied
	}
	return nil
}

func ensureCreateOwner(actor previewtunnelapi.Actor, ownerMachineID, ownerSessionID string) error {
	ownerMachineID = strings.TrimSpace(ownerMachineID)
	ownerSessionID = strings.TrimSpace(ownerSessionID)
	if ownerMachineID == "" || ownerSessionID == "" {
		return ErrOwnerDenied
	}
	if actor.MachineID != "" || actor.HostID != "" {
		if actor.MachineID != "" && actor.MachineID != ownerMachineID {
			return ErrOwnerDenied
		}
		if actor.HostID != "" && actor.HostID != ownerMachineID {
			return ErrOwnerDenied
		}
	}
	return nil
}

func actorType(actor previewtunnelapi.Actor) string {
	if actor.HostID != "" {
		return "host"
	}
	if actor.Role == "system_worker" {
		return "system"
	}
	if actor.Role == "edge" {
		return "edge"
	}
	return "user"
}

func validLocalTarget(raw string) bool {
	return previewtunnelstore.ValidPreviewTargetV1("http", raw, "public")
}

func normalizeEndpointDomain(raw string) (string, error) {
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if domain == "" || strings.ContainsAny(domain, "/:@?#") || strings.Contains(domain, "://") {
		return "", fmt.Errorf("%w: endpoint domain must be a host name", ErrInvalidInput)
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("%w: endpoint domain must contain at least two labels", ErrInvalidInput)
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("%w: endpoint domain label is invalid", ErrInvalidInput)
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", fmt.Errorf("%w: endpoint domain label is invalid", ErrInvalidInput)
			}
		}
	}
	return domain, nil
}

func hashCreateRequest(input CreateRequest) [sha256.Size]byte {
	payload := struct {
		OwnerMachineID   string     `json:"owner_machine_id"`
		OwnerSessionID   string     `json:"owner_session_id"`
		OwnerSessionKind string     `json:"owner_session_kind"`
		Target           Target     `json:"target"`
		AccessMode       string     `json:"access_mode,omitempty"`
		ExpiresAt        *time.Time `json:"expires_at"`
		Domains          []string   `json:"domains"`
	}{input.OwnerMachineID, input.OwnerSessionID, input.OwnerSessionKind, input.Target, input.AccessMode, input.ExpiresAt, input.Domains}
	encoded, _ := json.Marshal(payload)
	return sha256.Sum256(encoded)
}

func hashMutationRequest(previewID string, generation int64, deadline *time.Time) [sha256.Size]byte {
	payload := struct {
		PreviewID  string     `json:"preview_id"`
		Generation int64      `json:"generation"`
		Deadline   *time.Time `json:"deadline"`
	}{previewID, generation, deadline}
	encoded, _ := json.Marshal(payload)
	return sha256.Sum256(encoded)
}

func isEndpointConflict(err error) bool {
	return unaryPreviewCause(err, previewtunnelstore.ErrEndpointConflict)
}

func nullableString(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func randomID(source io.Reader, prefix string) (string, error) {
	value, err := uuid.NewRandomFromReader(source)
	if err != nil {
		return "", err
	}
	return prefix + "_" + value.String(), nil
}

type previewCursor struct {
	Version   int       `json:"version"`
	Kind      string    `json:"kind"`
	AccountID string    `json:"account_id"`
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

const previewCursorVersion = 1

type cursorCodec struct{ key []byte }

func newCursorCodec(key []byte) (*cursorCodec, error) {
	if len(key) < 32 {
		return nil, errors.New("preview cursor key must contain at least 32 bytes")
	}
	return &cursorCodec{key: append([]byte(nil), key...)}, nil
}

func (c *cursorCodec) Encode(value previewCursor) (string, error) {
	if value.AccountID == "" || value.ID == "" || value.CreatedAt.IsZero() {
		return "", previewtunnelapi.ErrInvalidCursor
	}
	value.Version = previewCursorVersion
	value.Kind = Kind
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(payload) > maxPreviewCursorPayload {
		return "", previewtunnelapi.ErrInvalidCursor
	}
	mac := hmac.New(sha256.New, c.key)
	_, _ = mac.Write(payload)
	raw := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(raw) > maxPreviewCursorEncoded {
		return "", previewtunnelapi.ErrInvalidCursor
	}
	return raw, nil
}

func (c *cursorCodec) Decode(raw, accountID string) (previewCursor, error) {
	if len(raw) == 0 || len(raw) > maxPreviewCursorEncoded {
		return previewCursor{}, previewtunnelapi.ErrInvalidCursor
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return previewCursor{}, previewtunnelapi.ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(payload) == 0 || len(payload) > maxPreviewCursorPayload {
		return previewCursor{}, previewtunnelapi.ErrInvalidCursor
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size {
		return previewCursor{}, previewtunnelapi.ErrInvalidCursor
	}
	mac := hmac.New(sha256.New, c.key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return previewCursor{}, previewtunnelapi.ErrInvalidCursor
	}
	var value previewCursor
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || decoder.Decode(&struct{}{}) != io.EOF || value.Version != previewCursorVersion || value.Kind != Kind || value.AccountID != accountID || value.ID == "" || value.CreatedAt.IsZero() {
		return previewCursor{}, previewtunnelapi.ErrInvalidCursor
	}
	return value, nil
}

// Management resolves the persisted owner from the authenticated actor's current
// machine grant. Machine proofs instead identify the exact target for cleanup.
func (s *Service) managementLeaseRequest(ctx context.Context, request previewtunnelapi.RequestContext, preview string) (previewtunnelapi.RequestContext, error) {
	if request.Actor.HostID != "" {
		return s.machineLeaseRequest(ctx, request, preview)
	}
	account, err := s.repository.ResolvePreviewManagementAccountV1(ctx, request.Actor.AccountID, preview)
	if err != nil {
		return request, err
	}
	request.Actor.AccountID = account
	return request, nil
}

// Machine proofs retain the enrollment issuer while the lease retains its
// creator's account. Resource ownership is never taken from request JSON.
func (s *Service) machineLeaseRequest(ctx context.Context, request previewtunnelapi.RequestContext, preview string) (previewtunnelapi.RequestContext, error) {
	machine := request.Actor.MachineID
	if request.Actor.HostID != "" {
		if machine != "" && machine != request.Actor.HostID {
			return request, ErrOwnerDenied
		}
		machine = request.Actor.HostID
	}
	if machine == "" {
		return request, nil
	}
	account, err := s.repository.ResolvePreviewLeaseMachineAccountV1(ctx, request.Actor.AccountID, machine, preview)
	if unaryPreviewCause(err, previewtunnelstore.ErrOwnerNotFound) {
		return request, &previewOwnerDenial{cause: err}
	}
	if err != nil {
		return request, err
	}
	request.Actor.AccountID = account
	return request, nil
}

func lazyLeaseRecordAuthority(current previewtunnelstore.PreviewLeaseRecord) previewtunnelstore.LazyLeaseAuthority {
	return previewtunnelstore.LazyLeaseAuthority{AccountID: current.AccountID, MachineID: current.OwnerMachineID, OwnerSessionID: current.OwnerSessionID, OwnerSessionKind: current.OwnerSessionKind, TargetScheme: current.TargetScheme, TargetAddress: current.TargetAddress, Endpoint: current.Endpoint, PreviewID: current.ID}
}
func (s *Service) verifyOwner(ctx context.Context, b previewtunnelstore.LazyLeaseAuthority, mode string) (bool, error) {
	if b.OwnerSessionKind != "lazy_runtime" {
		return s.repository.VerifyPreviewLeaseOwnerV1(ctx, b.AccountID, b.MachineID, mode)
	}
	if mode == "public" {
		return false, previewtunnelstore.ErrPreviewPublicationDenied
	}
	verifier, ok := s.repository.(interface {
		VerifyLazyPreviewLeaseV1(context.Context, previewtunnelstore.LazyLeaseAuthority) (bool, error)
	})
	if !ok {
		return false, ErrOwnerDenied
	}
	return verifier.VerifyLazyPreviewLeaseV1(ctx, b)
}

func workspaceCursorPrincipal(ctx context.Context, account string) string {
	if scope, ok := workspace.FromContext(ctx); ok {
		return workspace.InventoryCursorPrincipal(ctx, account+"\x1f"+scope.ID)
	}
	return workspace.InventoryCursorPrincipal(ctx, account)
}

// unaryPreviewCause permits owned sentinel decisions only through a complete
// single cause chain. Independent joined failures and unresolved chains cannot
// authorize a retry or be downgraded to a user denial.
func unaryPreviewCause(err, target error) (matched bool) {
	defer func() {
		if recover() != nil {
			matched = false
		}
	}()
	for depth := 0; err != nil && depth < 32; depth++ {
		if err == target {
			return true
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapper.Unwrap()
	}
	return false
}

type previewOwnerDenial struct{ cause error }

func (*previewOwnerDenial) Error() string        { return "preview lease ownership denied" }
func (e *previewOwnerDenial) Unwrap() error      { return e.cause }
func (*previewOwnerDenial) Is(target error) bool { return target == ErrOwnerDenied }
