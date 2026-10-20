package tunnelv1

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"github.com/google/uuid"

	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/naming"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelendpoint"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

var tunnelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

type Service struct {
	repository       TunnelRepository
	endpoint         EndpointBuilder
	cursors          *tunnelCursorCodec
	now              func() time.Time
	newID            func(prefix string) (string, error)
	newEndpointID    func() (string, error)
	publicTCPPortMin int32
	publicTCPPortMax int32
	bandwidthDenied  func(ctx context.Context, accountID string, at time.Time) bool
}

// API is the narrow lifecycle surface consumed by the HTTP router. Route,
// domain, connector, events, and logs remain separate follow-up boundaries.
type API interface {
	CreateTunnel(context.Context, previewtunnelapi.RequestContext, CreateTunnelRequest) (MutationResult, error)
	ListTunnels(context.Context, previewtunnelapi.RequestContext, string, int) (TunnelPage, error)
	GetTunnel(context.Context, previewtunnelapi.RequestContext, string) (TunnelView, error)
	PatchTunnel(context.Context, previewtunnelapi.RequestContext, string, PatchTunnelRequest) (MutationResult, error)
	PauseTunnel(context.Context, previewtunnelapi.RequestContext, string, MutationInput) (MutationResult, error)
	ResumeTunnel(context.Context, previewtunnelapi.RequestContext, string, MutationInput) (MutationResult, error)
	DeleteTunnel(context.Context, previewtunnelapi.RequestContext, string, MutationInput) (MutationResult, error)
	Status(context.Context, previewtunnelapi.RequestContext, string) (HealthView, error)
}

func NewService(repository TunnelRepository, config Config) (*Service, error) {
	if repository == nil {
		return nil, errors.New("tunnel repository is required")
	}
	if config.EndpointBuilder == nil {
		return nil, errors.New("tunnel endpoint builder is required")
	}
	cursors, err := newTunnelCursorCodec(config.CursorKey)
	if err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	idGenerator := config.NewID
	if idGenerator == nil {
		idGenerator = randomID
	}
	endpointIDGenerator := config.NewEndpointID
	if endpointIDGenerator == nil {
		endpointIDGenerator = randomEndpointID
	}
	if config.PublicTCPPortMin == 0 && config.PublicTCPPortMax == 0 {
		config.PublicTCPPortMin, config.PublicTCPPortMax = 20000, 29999
	}
	if config.PublicTCPPortMin < 1024 || config.PublicTCPPortMax > 65535 || config.PublicTCPPortMin > config.PublicTCPPortMax {
		return nil, errors.New("public TCP port range is invalid")
	}
	return &Service{repository: repository, endpoint: config.EndpointBuilder, cursors: cursors, now: now, newID: idGenerator, newEndpointID: endpointIDGenerator, publicTCPPortMin: config.PublicTCPPortMin, publicTCPPortMax: config.PublicTCPPortMax, bandwidthDenied: config.BandwidthDenied}, nil
}

// NewEndpointBuilder creates a deployment-specific stable hostname builder.
// The base URL must be HTTPS and must not include credentials, a port, path,
// query, or fragment. The readable hostname is random and independent of both
// the endpoint UUID and the user-controlled tunnel name.
func NewEndpointBuilder(baseURL string) (EndpointBuilder, error) {
	return newEndpointBuilder(baseURL, rand.Reader)
}

func newEndpointBuilder(baseURL string, random io.Reader) (EndpointBuilder, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Port() != "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("tunnel endpoint base URL must be an HTTPS origin without credentials, port, or path")
	}
	baseHost := strings.ToLower(parsed.Hostname())
	if err := validateDNSName(baseHost); err != nil {
		return nil, fmt.Errorf("invalid tunnel endpoint base host: %w", err)
	}
	return func(_ string, stableEndpointID string) (string, error) {
		if err := validateEndpointID(stableEndpointID); err != nil {
			return "", fmt.Errorf("tunnel endpoint identity is invalid: %w", err)
		}
		label, err := naming.PublicSlug(random)
		if err != nil {
			return "", err
		}
		return "https://" + label + "." + baseHost, nil
	}, nil
}

func (s *Service) CreateTunnel(ctx context.Context, request previewtunnelapi.RequestContext, input CreateTunnelRequest) (MutationResult, error) {
	if err := s.authorize(request, true, "write"); err != nil {
		return MutationResult{}, err
	}
	now := s.now().UTC()
	if err := s.repository.VerifyHost(ctx, request.Actor.AccountID, request.Actor.HostID); err != nil {
		return MutationResult{}, err
	}
	name, err := validateTunnelName(input.Name)
	if err != nil {
		return MutationResult{}, err
	}
	accessMode, err := normalizeAccessMode(input.AccessMode)
	if err != nil {
		return MutationResult{}, err
	}
	origin, err := normalizeOriginRequest(input.Origin)
	if err != nil {
		return MutationResult{}, err
	}
	input.Origin = origin
	if input.ExpiresAt != nil && !input.ExpiresAt.After(now) {
		return MutationResult{}, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidInput)
	}
	if err := validateMutationInput(input.MutationInput, false); err != nil {
		return MutationResult{}, err
	}
	tunnelID, err := s.newID("tunnel")
	if err != nil {
		return MutationResult{}, fmt.Errorf("allocate tunnel identity: %w", err)
	}
	endpointID, err := s.newEndpointID()
	if err != nil {
		return MutationResult{}, fmt.Errorf("allocate tunnel endpoint identity: %w", err)
	}
	if err := validateEndpointID(endpointID); err != nil {
		return MutationResult{}, fmt.Errorf("allocate tunnel endpoint identity: %w", err)
	}
	endpoint, err := s.endpoint(name, endpointID)
	if err != nil {
		return MutationResult{}, fmt.Errorf("allocate stable tunnel endpoint: %w", err)
	}
	if err := validateStableEndpointForID(endpoint, endpointID); err != nil {
		return MutationResult{}, err
	}
	operationID, err := s.newID("operation")
	if err != nil {
		return MutationResult{}, fmt.Errorf("allocate tunnel operation identity: %w", err)
	}
	auditID, err := s.newID("event")
	if err != nil {
		return MutationResult{}, fmt.Errorf("allocate tunnel audit identity: %w", err)
	}
	request, err = normalizeRequestContext(request, s.newID)
	if err != nil {
		return MutationResult{}, err
	}
	var expiresAt sql.NullTime
	if input.ExpiresAt != nil {
		expiresAt = sql.NullTime{Time: input.ExpiresAt.UTC(), Valid: true}
	}
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			endpoint, err = s.endpoint(name, endpointID)
			if err != nil {
				return MutationResult{}, fmt.Errorf("allocate stable tunnel endpoint: %w", err)
			}
			if err = validateStableEndpointForID(endpoint, endpointID); err != nil {
				return MutationResult{}, err
			}
		}
		result, err := s.repository.Create(ctx, CreateRecord{
			OperationID: operationID, TunnelID: tunnelID, StableEndpointID: endpointID, StableEndpoint: endpoint,
			AccountID: request.Actor.AccountID, Name: name, AccessMode: accessMode, Origin: input.Origin,
			ExpiresAt: expiresAt, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash,
			ActorID: request.Actor.ActorID, AuditActorID: auditActorIDForActor(request.Actor), ActorType: auditActorType(request.Actor), HostID: request.Actor.HostID,
			RequestID: request.RequestID, CorrelationID: request.CorrelationID, SourceMachineID: request.Actor.MachineID,
			AuditEventID: auditID, PublicTCPPortMin: s.publicTCPPortMin, PublicTCPPortMax: s.publicTCPPortMax, NewID: s.newID,
		})
		if errors.Is(err, ErrEndpointConflict) {
			continue
		}
		if err != nil {
			return MutationResult{}, err
		}
		return s.mutationView(result, request.RequestID), nil
	}
	return MutationResult{}, ErrEndpointConflict
}

func (s *Service) ListTunnels(ctx context.Context, request previewtunnelapi.RequestContext, rawCursor string, limit int) (TunnelPage, error) {
	if _, selected := workspace.FromContext(ctx); !selected {
		ctx = workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
	}
	if err := s.authorize(request, false, "read"); err != nil {
		return TunnelPage{}, err
	}
	if limit < 1 || limit > previewtunnelapi.MaximumPageLimit {
		return TunnelPage{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidInput, previewtunnelapi.MaximumPageLimit)
	}
	var after *ListPosition
	if strings.TrimSpace(rawCursor) != "" {
		position, err := s.cursors.Decode(rawCursor, workspaceCursorPrincipal(ctx, request.Actor.AccountID))
		if err != nil {
			return TunnelPage{}, err
		}
		after = &position
	}
	rows, err := s.repository.List(ctx, request.Actor.AccountID, after, limit+1)
	if err != nil {
		return TunnelPage{}, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	page := TunnelPage{Items: make([]TunnelView, 0, len(rows))}
	for _, row := range rows {
		page.Items = append(page.Items, tunnelView(row))
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		cursor, cursorErr := s.cursors.Encode(workspaceCursorPrincipal(ctx, request.Actor.AccountID), ListPosition{CreatedAt: last.CreatedAt, ID: last.ID})
		if cursorErr != nil {
			return TunnelPage{}, cursorErr
		}
		page.NextCursor = cursor
	}
	return page, nil
}

func (s *Service) GetTunnel(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID string) (TunnelView, error) {
	if err := s.authorize(request, false, "read"); err != nil {
		return TunnelView{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return TunnelView{}, scopeErr
	}
	tunnel, err := s.repository.Get(ctx, request.Actor.AccountID, tunnelID)
	if err != nil {
		return TunnelView{}, err
	}
	return tunnelView(tunnel), nil
}

func (s *Service) PatchTunnel(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID string, input PatchTunnelRequest) (MutationResult, error) {
	if err := s.authorize(request, false, "write"); err != nil {
		return MutationResult{}, err
	}
	ctx, request, shared, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return MutationResult{}, scopeErr
	}
	if shared && input.AccessMode != nil && strings.TrimSpace(*input.AccessMode) == AccessPublic {
		return MutationResult{}, ErrMachinePublicationDenied
	}
	now := s.now().UTC()
	if err := validateMutationInput(input.MutationInput, true); err != nil {
		return MutationResult{}, err
	}
	if input.Name != nil {
		name, err := validateTunnelName(*input.Name)
		if err != nil {
			return MutationResult{}, err
		}
		input.Name = &name
	}
	if input.AccessMode != nil {
		if strings.TrimSpace(*input.AccessMode) == "" {
			return MutationResult{}, fmt.Errorf("%w: access_mode cannot be empty", ErrInvalidInput)
		}
		mode, err := normalizeAccessMode(*input.AccessMode)
		if err != nil {
			return MutationResult{}, err
		}
		input.AccessMode = &mode
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(now) {
		return MutationResult{}, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidInput)
	}
	if input.Name == nil && input.AccessMode == nil && !input.ExpirySet {
		return MutationResult{}, fmt.Errorf("%w: patch must contain a mutable field", ErrInvalidInput)
	}
	request, normalizeErr := normalizeRequestContext(request, s.newID)
	if normalizeErr != nil {
		return MutationResult{}, normalizeErr
	}
	var err error
	operationID, err := s.newID("operation")
	if err != nil {
		return MutationResult{}, err
	}
	auditID, err := s.newID("event")
	if err != nil {
		return MutationResult{}, err
	}
	result, err := s.repository.Patch(ctx, PatchRecord{
		OperationID: operationID, AuditEventID: auditID, TunnelID: tunnelID, AccountID: request.Actor.AccountID,
		Name: input.Name, AccessMode: input.AccessMode, ExpiresAt: input.ExpiresAt, ExpirySet: input.ExpirySet,
		ExpectedGeneration: input.ExpectedGeneration, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash,
		ActorID: request.Actor.ActorID, AuditActorID: auditActorIDForActor(request.Actor), ActorType: auditActorType(request.Actor), RequestID: request.RequestID,
		CorrelationID: request.CorrelationID, SourceMachineID: request.Actor.MachineID, Now: now,
	})
	if err != nil {
		return MutationResult{}, err
	}
	return s.mutationView(result, request.RequestID), nil
}

func (s *Service) PauseTunnel(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID string, input MutationInput) (MutationResult, error) {
	return s.transition(ctx, request, tunnelID, DesiredPaused, input)
}

func (s *Service) ResumeTunnel(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID string, input MutationInput) (MutationResult, error) {
	return s.transition(ctx, request, tunnelID, DesiredActive, input)
}

func (s *Service) DeleteTunnel(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID string, input MutationInput) (MutationResult, error) {
	return s.transition(ctx, request, tunnelID, DesiredDeleted, input)
}

func (s *Service) transition(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID, desiredState string, input MutationInput) (MutationResult, error) {
	if err := s.authorize(request, false, "write"); err != nil {
		return MutationResult{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return MutationResult{}, scopeErr
	}
	if err := validateMutationInput(input, true); err != nil {
		return MutationResult{}, err
	}
	request, normalizeErr := normalizeRequestContext(request, s.newID)
	if normalizeErr != nil {
		return MutationResult{}, normalizeErr
	}
	var err error
	operationID, err := s.newID("operation")
	if err != nil {
		return MutationResult{}, err
	}
	auditID, err := s.newID("event")
	if err != nil {
		return MutationResult{}, err
	}
	result, err := s.repository.Transition(ctx, StateRecord{
		OperationID: operationID, AuditEventID: auditID, TunnelID: tunnelID, AccountID: request.Actor.AccountID,
		DesiredState: desiredState, ExpectedGeneration: input.ExpectedGeneration, IdempotencyKey: input.IdempotencyKey,
		RequestHash: input.RequestHash, ActorID: request.Actor.ActorID, AuditActorID: auditActorIDForActor(request.Actor), ActorType: auditActorType(request.Actor),
		RequestID: request.RequestID, CorrelationID: request.CorrelationID, SourceMachineID: request.Actor.MachineID,
	})
	if err != nil {
		return MutationResult{}, err
	}
	return s.mutationView(result, request.RequestID), nil
}

func (s *Service) Status(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID string) (HealthView, error) {
	if err := s.authorize(request, false, "read"); err != nil {
		return HealthView{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return HealthView{}, scopeErr
	}
	tunnel, err := s.repository.Get(ctx, request.Actor.AccountID, tunnelID)
	if err != nil {
		return HealthView{}, err
	}
	request, err = normalizeRequestContext(request, s.newID)
	if err != nil {
		return HealthView{}, err
	}
	now := s.now().UTC()
	view := projectHealth(tunnel, request.CorrelationID, now)
	facts, err := s.repository.HealthFacts(ctx, request.Actor.AccountID, tunnelID, now)
	if err != nil {
		return HealthView{}, err
	}
	if s.bandwidthDenied != nil {
		facts.BandwidthExhausted = s.bandwidthDenied(ctx, request.Actor.AccountID, now)
	}
	return explainHealth(view, facts), nil
}

// ReconcileExpired performs the server-side expiry sweep. Expiry changes only
// the computed summary and operation/audit state; desired_state and stable
// identity remain available for an explicit extension or delete.
func (s *Service) ReconcileExpired(ctx context.Context, input ExpiryReconcileRequest) ([]MutationResult, error) {
	if input.Limit == 0 {
		input.Limit = 100
	}
	if input.Now.IsZero() {
		input.Now = s.now().UTC()
	}
	var err error
	request := previewtunnelapi.RequestContext{RequestID: input.RequestID, CorrelationID: input.CorrelationID}
	request, err = normalizeRequestContext(request, s.newID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.ActorID) == "" {
		return nil, fmt.Errorf("%w: expiry actor id is required", ErrInvalidInput)
	}
	if input.ActorType == "" {
		input.ActorType = "system"
	}
	records, err := s.repository.ReconcileExpired(ctx, ExpiryRecord{
		Now: input.Now, Limit: input.Limit, ActorID: input.ActorID, ActorType: input.ActorType,
		RequestID: request.RequestID, CorrelationID: request.CorrelationID, NewID: s.newID,
	})
	if err != nil {
		return nil, err
	}
	results := make([]MutationResult, 0, len(records))
	for _, record := range records {
		results = append(results, s.mutationView(record, request.RequestID))
	}
	return results, nil
}

func (s *Service) authorize(request previewtunnelapi.RequestContext, requireHost bool, action string) error {
	if err := previewtunnelapi.Authorize(request.Actor, previewtunnelapi.AccessRequest{
		AccountID: request.Actor.AccountID, Resource: "tunnels", Action: action, RequireHost: requireHost,
	}); err != nil {
		return err
	}
	if requireHost && request.Actor.MachineID != request.Actor.HostID {
		// The machine verifier supplies one structural machine identity. Keep
		// the two fields equal at this boundary so a caller cannot combine a
		// valid host ID with an unrelated machine/session identity.
		return previewtunnelapi.ErrHostActorRequired
	}
	return nil
}

func (s *Service) mutationView(record MutationRecord, requestID string) MutationResult {
	return MutationResult{
		Tunnel: tunnelView(record.Tunnel), Operation: previewtunnelapi.OperationView(record.Operation, requestID),
		Replayed: record.Replayed, Changed: record.Changed,
	}
}

func tunnelView(row dbsqlc.Tunnel) TunnelView {
	view := TunnelView{
		Schema: Schema, Kind: "tunnel", ID: row.ID, AccountID: row.AccountID, Name: row.Name,
		DesiredState: row.DesiredState, AccessMode: row.AccessMode, Generation: row.Generation,
		ETag: previewtunnelapi.ETag("tunnel", row.ID, row.Generation), StableEndpointID: row.StableEndpointID,
		StableEndpoint: row.StableEndpoint, CreatedByHostID: row.CreatedByHostID,
		CreatedByActorID: row.CreatedByActorID, SummaryCode: row.SummaryCode,
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
	if row.ExpiresAt.Valid {
		expiresAt := row.ExpiresAt.Time.UTC()
		view.ExpiresAt = &expiresAt
	}
	return view
}

func projectHealth(tunnel dbsqlc.Tunnel, correlationID string, now time.Time) HealthView {
	if correlationID == "" {
		correlationID = "cor_unknown"
	}
	dimensions := HealthDimensions{
		Service:     HealthDimension{Status: "unknown", Code: "connector_pending"},
		Edge:        HealthDimension{Status: "unknown", Code: "edge_pending"},
		Config:      HealthDimension{Status: "ready", Code: "desired_state_persisted"},
		Route:       HealthDimension{Status: "ready", Code: "route_persisted"},
		Origin:      HealthDimension{Status: "unknown", Code: "origin_pending"},
		DNS:         HealthDimension{Status: "ready", Code: "managed_endpoint"},
		Certificate: HealthDimension{Status: "ready", Code: "edge_certificate"},
		Access:      HealthDimension{Status: "ready", Code: "access_" + tunnel.AccessMode},
		Update:      HealthDimension{Status: "not_applicable", Code: "no_update"},
	}
	overallCode := tunnel.SummaryCode
	if overallCode == "" || overallCode == "pending" {
		overallCode = "tunnel_pending"
	}
	summary := "Tunnel is waiting for a connector and origin readiness check."
	retrying := true
	repairAction := "Check that the Paperboat host service and local app are running, then wait for readiness."
	if tunnel.ExpiresAt.Valid && !tunnel.ExpiresAt.Time.After(now) && tunnel.DesiredState != DesiredDeleted {
		overallCode = "tunnel_expired"
		summary = "Tunnel expiry has passed; new traffic must be rejected."
		retrying = false
		repairAction = "Extend the expiry or delete the tunnel."
		dimensions.Service = HealthDimension{Status: "down", Code: "tunnel_expired"}
		dimensions.Edge = HealthDimension{Status: "down", Code: "tunnel_expired"}
		dimensions.Route = HealthDimension{Status: "down", Code: "route_expired"}
		dimensions.Origin = HealthDimension{Status: "down", Code: "origin_blocked_expired"}
	} else if tunnel.DesiredState == DesiredDeleted {
		overallCode = "tunnel_deleted"
		summary = "Tunnel is deleted and no longer accepts traffic."
		retrying = false
		repairAction = "Create a new tunnel if the service should be exposed again."
		dimensions.Service = HealthDimension{Status: "down", Code: "tunnel_deleted"}
		dimensions.Edge = HealthDimension{Status: "down", Code: "tunnel_deleted"}
		dimensions.Config = HealthDimension{Status: "not_applicable", Code: "configuration_revoked"}
		dimensions.Route = HealthDimension{Status: "not_applicable", Code: "routes_revoked"}
		dimensions.Origin = HealthDimension{Status: "not_applicable", Code: "origin_not_reached"}
	} else if tunnel.DesiredState == DesiredPaused {
		overallCode = "tunnel_paused"
		summary = "Tunnel is paused; existing identity and routes are preserved."
		retrying = false
		repairAction = "Resume the tunnel when it should accept new traffic."
		dimensions.Edge = HealthDimension{Status: "degraded", Code: "traffic_paused"}
		dimensions.Route = HealthDimension{Status: "degraded", Code: "route_paused"}
	} else if tunnel.SummaryCode == "custom_domain_required" {
		overallCode = "custom_domain_required"
		summary = "The self-hosted tunnel needs a verified custom domain before it can accept traffic."
		retrying = false
		repairAction = "Add a custom domain to the route and configure the shown DNS records."
		dimensions.DNS = HealthDimension{Status: "down", Code: "custom_domain_required"}
		dimensions.Certificate = HealthDimension{Status: "unknown", Code: "custom_domain_pending"}
		dimensions.Route = HealthDimension{Status: "down", Code: "custom_domain_required"}
	} else if tunnel.SummaryCode == "route_unavailable" {
		overallCode = "route_unavailable"
		summary = "The edge could not forward a tunnel route."
		repairAction = "Check the origin service and its configured address."
		dimensions.Service = HealthDimension{Status: "ready", Code: "connector_ready"}
		dimensions.Edge = HealthDimension{Status: "ready", Code: "edge_connected"}
		dimensions.Origin = HealthDimension{Status: "unknown", Code: "origin_unverified"}
	} else if tunnel.SummaryCode == "origin_unreachable" {
		overallCode = "origin_unreachable"
		summary = "The tunnel is connected but its local app is not reachable."
		repairAction = "The connector cannot reach the app behind this tunnel. Start it, then wait a few seconds."
		dimensions.Service = HealthDimension{Status: "ready", Code: "connector_ready"}
		dimensions.Edge = HealthDimension{Status: "ready", Code: "edge_connected"}
		dimensions.Origin = HealthDimension{Status: "down", Code: "origin_unreachable"}
	} else if tunnel.SummaryCode == "ready" {
		overallCode = "ready"
		summary = "Tunnel is ready to accept traffic."
		retrying = false
		repairAction = "No action required."
		dimensions.Service = HealthDimension{Status: "ready", Code: "connector_ready"}
		dimensions.Edge = HealthDimension{Status: "ready", Code: "edge_connected"}
		dimensions.Origin = HealthDimension{Status: "ready", Code: "origin_ready"}
	}
	since := tunnel.SummaryTransitionedAt.UTC()
	if since.IsZero() {
		since = tunnel.UpdatedAt.UTC()
	}
	return HealthView{
		Schema: Schema, Kind: "health", ResourceKind: "tunnel", ResourceID: tunnel.ID,
		OverallCode: overallCode, Dimensions: dimensions, Summary: summary, Since: since,
		Retrying: retrying, RepairAction: repairAction, CorrelationID: correlationID,
	}
}

// TunnelAdmissionAllowed is the durable data-plane gate. A reconciler or edge
// must evaluate expires_at and desired_state at request time; summary_code is
// only an indexed health projection and is not sufficient to authorize new
// traffic during a delayed expiry sweep.
func TunnelAdmissionAllowed(tunnel dbsqlc.Tunnel, now time.Time) bool {
	if tunnel.DesiredState != DesiredActive {
		return false
	}
	return !tunnel.ExpiresAt.Valid || tunnel.ExpiresAt.Time.After(now.UTC())
}

func validateMutationInput(input MutationInput, requireGeneration bool) error {
	if strings.TrimSpace(input.IdempotencyKey) == "" || len(input.IdempotencyKey) > 256 {
		return fmt.Errorf("%w: idempotency key is required", ErrInvalidInput)
	}
	if input.RequestHash == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: request hash is required", ErrInvalidInput)
	}
	if requireGeneration && input.ExpectedGeneration < 1 {
		return fmt.Errorf("%w: expected generation is required", ErrInvalidInput)
	}
	return nil
}

func validateTunnelName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !tunnelNamePattern.MatchString(name) {
		return "", fmt.Errorf("%w: name must be 1-63 ASCII letters, digits, '.', '_' or '-' and cannot start with punctuation", ErrInvalidInput)
	}
	return name, nil
}

func normalizeAccessMode(value string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return AccessPublic, nil
	}
	if value != AccessPublic && value != AccessPrivate && value != "team" {
		return "", fmt.Errorf("%w: access_mode must be public, private or team", ErrInvalidInput)
	}
	return value, nil
}

func validateOriginRequest(origin OriginRequest) error {
	if len(origin.Address) == 0 || len(origin.Address) > 512 || origin.Address != strings.TrimSpace(origin.Address) || strings.ContainsAny(origin.Address, "\r\n\t\x00") {
		return fmt.Errorf("%w: origin.address is not canonical", ErrInvalidInput)
	}
	switch origin.Scheme {
	case "http", "https", "h2c", "tcp":
		if err := validateHostPort(origin.Address); err != nil {
			return fmt.Errorf("%w: origin.address: %v", ErrInvalidInput, err)
		}
	case "unix":
		if !strings.HasPrefix(origin.Address, "/") || path.Clean(origin.Address) != origin.Address {
			return fmt.Errorf("%w: origin.address must be a clean absolute Unix socket path", ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: origin.scheme %q is not supported by tunnel v1 route persistence", ErrInvalidInput, origin.Scheme)
	}
	if origin.HostOverride != nil {
		if err := validateHostOnly(*origin.HostOverride); err != nil {
			return fmt.Errorf("%w: origin.host_override: %v", ErrInvalidInput, err)
		}
	}
	return nil
}

func normalizeOriginRequest(origin OriginRequest) (OriginRequest, error) {
	origin.Scheme = strings.ToLower(strings.TrimSpace(origin.Scheme))
	if origin.HostOverride != nil {
		normalized := strings.ToLower(strings.TrimSpace(*origin.HostOverride))
		origin.HostOverride = &normalized
	}
	if err := validateOriginRequest(origin); err != nil {
		return OriginRequest{}, err
	}
	return origin, nil
}

func validateHostPort(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("must contain a host and a numeric port")
	}
	if portText == "" || (len(portText) > 1 && portText[0] == '0') {
		return errors.New("port must be canonical")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return errors.New("port must be between 1 and 65535")
	}
	if err := validateHostOnly(host); err != nil {
		return err
	}
	return nil
}

func validateHostOnly(host string) error {
	if host == "" || host != strings.TrimSpace(host) || strings.ContainsAny(host, "\r\n\t /?#@") {
		return errors.New("host is invalid")
	}
	if net.ParseIP(host) != nil || strings.EqualFold(host, "localhost") {
		return nil
	}
	if len(host) > 253 || strings.HasSuffix(host, ".") {
		return errors.New("host is invalid")
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("host is invalid")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return errors.New("host is invalid")
			}
		}
	}
	return nil
}

func validateStableEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Port() != "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("%w: stable endpoint must be an HTTPS host-only URL", ErrInvalidInput)
	}
	host := parsed.Hostname()
	if err := validateDNSName(host); err != nil {
		return fmt.Errorf("%w: stable endpoint host is invalid: %v", ErrInvalidInput, err)
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%w: stable endpoint must include a configured base domain", ErrInvalidInput)
	}

	return nil
}

func validateStableEndpointForID(endpoint, endpointID string) error {
	if err := validateEndpointID(endpointID); err != nil {
		return fmt.Errorf("%w: stable endpoint identity is invalid: %v", ErrInvalidInput, err)
	}
	if err := validateStableEndpoint(endpoint); err != nil {
		return err
	}

	return nil
}

func validateEndpointID(value string) error {
	return tunnelendpoint.ValidateID(value)
}

func validateDNSName(host string) error {
	if host == "" || len(host) > 253 || host != strings.ToLower(host) || net.ParseIP(host) != nil || strings.EqualFold(host, "localhost") {
		return errors.New("DNS name is invalid")
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("DNS label must contain 1-63 characters and cannot start or end with '-'")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return errors.New("DNS label contains an invalid character")
			}
		}
	}
	return nil
}

func randomID(prefix string) (string, error) {
	value, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return prefix + "_" + value.String(), nil
}

func randomEndpointID() (string, error) { return randomID("endpoint") }

func auditActorType(actor previewtunnelapi.Actor) string {
	if strings.TrimSpace(actor.HostID) != "" {
		return "host"
	}
	if actor.Role == "system_worker" {
		return "system"
	}
	return "user"
}

func auditActorIDForActor(actor previewtunnelapi.Actor) string {
	if strings.TrimSpace(actor.HostID) != "" {
		return actor.HostID
	}
	return actor.ActorID
}

type requestContextIDFailure struct{ cause error }

func (requestContextIDFailure) Error() string   { return "request context ID allocation failed" }
func (e requestContextIDFailure) Unwrap() error { return e.cause }

func normalizeRequestContext(request previewtunnelapi.RequestContext, newID func(prefix string) (string, error)) (previewtunnelapi.RequestContext, error) {
	if strings.TrimSpace(request.RequestID) == "" {
		value, err := newID("request")
		if err != nil {
			return previewtunnelapi.RequestContext{}, requestContextIDFailure{cause: err}
		}
		request.RequestID = value
	}
	if strings.TrimSpace(request.CorrelationID) == "" {
		value, err := newID("correlation")
		if err != nil {
			return previewtunnelapi.RequestContext{}, requestContextIDFailure{cause: err}
		}
		request.CorrelationID = value
	}
	return request, nil
}

func nullableStringValue(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func workspaceCursorPrincipal(ctx context.Context, account string) string {
	if scope, ok := workspace.FromContext(ctx); ok {
		return workspace.InventoryCursorPrincipal(ctx, account+"\x1f"+scope.ID)
	}
	return workspace.InventoryCursorPrincipal(ctx, account)
}
