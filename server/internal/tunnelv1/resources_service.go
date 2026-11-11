package tunnelv1

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
	"golang.org/x/net/idna"
)

type ResourceService struct {
	repository       ResourceRepository
	cursors          *resourceCursorCodec
	now              func() time.Time
	newID            func(prefix string) (string, error)
	publicTCPPortMin int32
	publicTCPPortMax int32
}

const maxResourceLogMessageBytes = 1000

func NewResourceService(repository ResourceRepository, config ResourceConfig) (*ResourceService, error) {
	if repository == nil {
		return nil, fmt.Errorf("resource repository is required")
	}
	cursors, err := newResourceCursorCodec(config.CursorKey)
	if err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newID := config.NewID
	if newID == nil {
		newID = randomID
	}
	if config.PublicTCPPortMin == 0 && config.PublicTCPPortMax == 0 {
		config.PublicTCPPortMin, config.PublicTCPPortMax = edgeports.PublicTCPMin, edgeports.PublicTCPMax
	}
	if config.PublicTCPPortMin < 1024 || config.PublicTCPPortMax > 65535 || config.PublicTCPPortMin > config.PublicTCPPortMax {
		return nil, fmt.Errorf("public TCP port range is invalid")
	}
	return &ResourceService{repository: repository, cursors: cursors, now: now, newID: newID, publicTCPPortMin: config.PublicTCPPortMin, publicTCPPortMax: config.PublicTCPPortMax}, nil
}

func (s *ResourceService) authorize(request previewtunnelapi.RequestContext, action string, requireHost bool) error {
	return previewtunnelapi.Authorize(request.Actor, previewtunnelapi.AccessRequest{AccountID: request.Actor.AccountID, Resource: "tunnels", Action: action, RequireHost: requireHost})
}

func (s *ResourceService) ListRoutes(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID, rawCursor string, limit int) (RoutePage, error) {
	if err := s.authorize(request, "read", false); err != nil {
		return RoutePage{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return RoutePage{}, scopeErr
	}
	if err := validateResourceListLimit(limit); err != nil {
		return RoutePage{}, err
	}
	position, err := s.decodeListCursor(rawCursor, request.Actor.AccountID, tunnelID, "route")
	if err != nil {
		return RoutePage{}, err
	}
	rows, err := s.repository.ListResourceRoutes(ctx, request.Actor.AccountID, tunnelID, position, limit+1)
	if err != nil {
		return RoutePage{}, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	page := RoutePage{Items: make([]RouteView, 0, len(rows))}
	for _, row := range rows {
		page.Items = append(page.Items, routeView(row))
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor, err = s.cursors.EncodeList("route", request.Actor.AccountID, tunnelID, ListPosition{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			return RoutePage{}, err
		}
	}
	return page, nil
}

func (s *ResourceService) GetRoute(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID, routeID string) (RouteView, error) {
	if err := s.authorize(request, "read", false); err != nil {
		return RouteView{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return RouteView{}, scopeErr
	}
	row, err := s.repository.GetResourceRoute(ctx, request.Actor.AccountID, tunnelID, routeID)
	if err != nil {
		return RouteView{}, err
	}
	return routeView(row), nil
}

func (s *ResourceService) CreateRoute(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID string, input RouteCreateRequest) (RouteMutationResult, error) {
	if err := s.authorize(request, "write", false); err != nil {
		return RouteMutationResult{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return RouteMutationResult{}, scopeErr
	}
	normalized, err := normalizeRouteCreate(input)
	if err != nil {
		return RouteMutationResult{}, err
	}
	request, err = normalizeRequestContext(request, s.newID)
	if err != nil {
		return RouteMutationResult{}, err
	}
	opID, auditID, parentAuditID, err := s.mutationIDs()
	if err != nil {
		return RouteMutationResult{}, err
	}
	routeID, err := s.newID("route")
	if err != nil {
		return RouteMutationResult{}, err
	}
	record := RouteRecord{OperationID: opID, AuditEventID: auditID, ParentAuditEventID: parentAuditID, AccountID: request.Actor.AccountID, TunnelID: tunnelID,
		RouteID: routeID, Name: normalized.Name, Protocol: dbProtocol(normalized.Protocol), MatchType: normalized.MatchType,
		Hostname: normalized.Hostname, WildcardSuffix: normalized.WildcardSuffix, PathPrefix: normalized.PathPrefix, Origin: normalized.Origin,
		Priority: normalized.Priority, ConnectTimeoutMS: normalized.ConnectTimeoutMS, IdleTimeoutMS: normalized.IdleTimeoutMS, MaxConcurrentStreams: normalized.MaxConcurrentStreams,
		DesiredState: "active", IdempotencyKey: input.Mutation.IdempotencyKey, RequestHash: input.Mutation.RequestHash,
		ActorID: request.Actor.ActorID, AuditActorID: auditActorIDForActor(request.Actor), ActorType: auditActorType(request.Actor), RequestID: request.RequestID,
		CorrelationID: request.CorrelationID, SourceMachineID: request.Actor.MachineID, Now: s.now().UTC(), RewriteRules: normalized.RewriteRules}
	if record.Protocol == "tcp" {
		record.PublicTCPListenerID, err = s.newID("listener")
		if err != nil {
			return RouteMutationResult{}, err
		}
		record.PublicTCPPortMin, record.PublicTCPPortMax = s.publicTCPPortMin, s.publicTCPPortMax
	}
	if record.RouteID == "" {
		return RouteMutationResult{}, fmt.Errorf("%w: route identity allocation failed", ErrInvalidInput)
	}
	result, err := s.repository.CreateResourceRoute(ctx, record)
	if err != nil {
		return RouteMutationResult{}, err
	}
	return RouteMutationResult{Route: routeView(result.Route), Operation: previewtunnelapi.OperationView(result.Operation, request.RequestID), Replayed: result.Replayed, Changed: result.Changed}, nil
}

func (s *ResourceService) PatchRoute(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID, routeID string, input RoutePatchRequest) (RouteMutationResult, error) {
	if err := s.authorize(request, "write", false); err != nil {
		return RouteMutationResult{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return RouteMutationResult{}, scopeErr
	}
	normalized, err := normalizeRoutePatch(input)
	if err != nil {
		return RouteMutationResult{}, err
	}
	request, err = normalizeRequestContext(request, s.newID)
	if err != nil {
		return RouteMutationResult{}, err
	}
	opID, auditID, parentAuditID, err := s.mutationIDs()
	if err != nil {
		return RouteMutationResult{}, err
	}
	record := RouteRecord{OperationID: opID, AuditEventID: auditID, ParentAuditEventID: parentAuditID, AccountID: request.Actor.AccountID, TunnelID: tunnelID, RouteID: routeID,
		Name: normalized.Name, Protocol: dbProtocol(normalized.Protocol), MatchType: normalized.MatchType, Hostname: normalized.Hostname, WildcardSuffix: normalized.WildcardSuffix,
		PathPrefix: normalized.PathPrefix, Origin: normalized.Origin, Priority: normalized.Priority, ConnectTimeoutMS: normalized.ConnectTimeoutMS, IdleTimeoutMS: normalized.IdleTimeoutMS,
		MaxConcurrentStreams: normalized.MaxConcurrentStreams, DesiredState: normalized.DesiredState, ExpectedGeneration: input.Mutation.ExpectedGeneration,
		IdempotencyKey: input.Mutation.IdempotencyKey, RequestHash: input.Mutation.RequestHash, ActorID: request.Actor.ActorID, AuditActorID: auditActorIDForActor(request.Actor), ActorType: auditActorType(request.Actor),
		RequestID: request.RequestID, CorrelationID: request.CorrelationID, SourceMachineID: request.Actor.MachineID, Now: s.now().UTC(), NameSet: input.Name != nil, ProtocolSet: input.Protocol != nil,
		MatchTypeSet: input.MatchType != nil, HostnameSet: input.Hostname != nil, WildcardSuffixSet: input.WildcardSuffix != nil, PathPrefixSet: input.PathPrefixSet, OriginSet: input.Origin != nil,
		PrioritySet: input.Priority != nil, ConnectTimeoutSet: input.ConnectTimeoutMS != nil, IdleTimeoutSet: input.IdleTimeoutMS != nil,
		MaxStreamsSet: input.MaxConcurrentStreams != nil, DesiredStateSet: input.DesiredState != nil,
		RewriteRules: normalized.RewriteRules, RewriteRulesSet: input.RewriteRulesSet}
	result, err := s.repository.PatchResourceRoute(ctx, record)
	if err != nil {
		return RouteMutationResult{}, err
	}
	return RouteMutationResult{Route: routeView(result.Route), Operation: previewtunnelapi.OperationView(result.Operation, request.RequestID), Replayed: result.Replayed, Changed: result.Changed}, nil
}

func (s *ResourceService) DeleteRoute(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID, routeID string, input ResourceMutationInput) (RouteMutationResult, error) {
	if err := s.authorize(request, "write", false); err != nil {
		return RouteMutationResult{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return RouteMutationResult{}, scopeErr
	}
	request, err := normalizeRequestContext(request, s.newID)
	if err != nil {
		return RouteMutationResult{}, err
	}
	opID, auditID, parentAuditID, err := s.mutationIDs()
	if err != nil {
		return RouteMutationResult{}, err
	}
	record := RouteRecord{OperationID: opID, AuditEventID: auditID, ParentAuditEventID: parentAuditID, AccountID: request.Actor.AccountID, TunnelID: tunnelID, RouteID: routeID,
		ExpectedGeneration: input.ExpectedGeneration, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash, ActorID: request.Actor.ActorID, AuditActorID: auditActorIDForActor(request.Actor), ActorType: auditActorType(request.Actor),
		RequestID: request.RequestID, CorrelationID: request.CorrelationID, SourceMachineID: request.Actor.MachineID, Now: s.now().UTC()}
	result, err := s.repository.DeleteResourceRoute(ctx, record)
	if err != nil {
		return RouteMutationResult{}, err
	}
	return RouteMutationResult{Route: routeView(result.Route), Operation: previewtunnelapi.OperationView(result.Operation, request.RequestID), Replayed: result.Replayed, Changed: result.Changed}, nil
}

func (s *ResourceService) ListDomains(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID, rawCursor string, limit int) (DomainPage, error) {
	if err := s.authorize(request, "read", false); err != nil {
		return DomainPage{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return DomainPage{}, scopeErr
	}
	if err := validateResourceListLimit(limit); err != nil {
		return DomainPage{}, err
	}
	position, err := s.decodeListCursor(rawCursor, request.Actor.AccountID, tunnelID, "domain_binding")
	if err != nil {
		return DomainPage{}, err
	}
	rows, err := s.repository.ListResourceDomains(ctx, request.Actor.AccountID, tunnelID, position, limit+1)
	if err != nil {
		return DomainPage{}, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	page := DomainPage{Items: make([]DomainView, 0, len(rows))}
	for _, row := range rows {
		page.Items = append(page.Items, DomainViewFromRow(row))
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor, err = s.cursors.EncodeList("domain_binding", request.Actor.AccountID, tunnelID, ListPosition{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			return DomainPage{}, err
		}
	}
	return page, nil
}

func (s *ResourceService) GetDomain(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID, domainID string) (DomainView, error) {
	if err := s.authorize(request, "read", false); err != nil {
		return DomainView{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return DomainView{}, scopeErr
	}
	row, err := s.repository.GetResourceDomain(ctx, request.Actor.AccountID, tunnelID, domainID)
	if err != nil {
		return DomainView{}, err
	}
	return DomainViewFromRow(row), nil
}

func (s *ResourceService) CreateDomain(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID string, input DomainCreateRequest) (DomainMutationResult, error) {
	if err := s.authorize(request, "write", false); err != nil {
		return DomainMutationResult{}, err
	}
	ctx, request, shared, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return DomainMutationResult{}, scopeErr
	}
	if shared {
		return DomainMutationResult{}, ErrMachinePublicationDenied
	}
	hostname, matchType, err := normalizeBindingHostname(input.Hostname)
	if err != nil {
		return DomainMutationResult{}, err
	}
	if strings.TrimSpace(input.RouteID) == "" {
		return DomainMutationResult{}, fmt.Errorf("%w: route_id is required", ErrInvalidInput)
	}
	route, err := s.repository.GetResourceRoute(ctx, request.Actor.AccountID, tunnelID, input.RouteID)
	if err != nil {
		return DomainMutationResult{}, err
	}
	if route.DeletedAt.Valid || route.DesiredState == "deleted" {
		return DomainMutationResult{}, ErrRouteNotFound
	}
	if (route.Protocol == "tcp" && (!route.PublicTcpPort.Valid || route.PublicTcpPort.Int32 < 1024)) || (route.Protocol == "tls" && (!route.PublicTcpPort.Valid || route.PublicTcpPort.Int32 != 443)) {
		return DomainMutationResult{}, fmt.Errorf("%w: public TCP/TLS route has no assigned listener port", ErrInvalidInput)
	}
	provider, err := validateDNSProvider(input.Provider)
	if err != nil {
		return DomainMutationResult{}, err
	}
	certificateStrategy, err := normalizeDomainCertificateStrategy(input.CertificateStrategy, matchType, route.Protocol)
	if err != nil {
		return DomainMutationResult{}, err
	}
	request, err = normalizeRequestContext(request, s.newID)
	if err != nil {
		return DomainMutationResult{}, err
	}
	opID, auditID, parentAuditID, err := s.mutationIDs()
	if err != nil {
		return DomainMutationResult{}, err
	}
	domainID, err := s.newID("domain")
	if err != nil {
		return DomainMutationResult{}, err
	}
	challengeID, err := s.newID("challenge")
	if err != nil {
		return DomainMutationResult{}, err
	}
	record := DomainRecord{OperationID: opID, AuditEventID: auditID, ParentAuditEventID: parentAuditID, AccountID: request.Actor.AccountID, TunnelID: tunnelID, DomainID: domainID, RouteID: input.RouteID,
		Hostname: hostname, MatchType: matchType, CertificateStrategy: certificateStrategy, ChallengeReference: "dns-challenge://" + challengeID, DNSProvider: provider, IdempotencyKey: input.Mutation.IdempotencyKey, RequestHash: input.Mutation.RequestHash,
		ActorID: request.Actor.ActorID, AuditActorID: auditActorIDForActor(request.Actor), ActorType: auditActorType(request.Actor), RequestID: request.RequestID, CorrelationID: request.CorrelationID,
		SourceMachineID: request.Actor.MachineID, Now: s.now().UTC()}
	result, err := s.repository.CreateResourceDomain(ctx, record)
	if err != nil {
		return DomainMutationResult{}, err
	}
	return DomainMutationResult{Domain: DomainViewFromRow(result.Domain), Operation: previewtunnelapi.OperationView(result.Operation, request.RequestID), Replayed: result.Replayed, Changed: result.Changed}, nil
}

func (s *ResourceService) DeleteDomain(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID, domainID string, input ResourceMutationInput) (DomainMutationResult, error) {
	if err := s.authorize(request, "write", false); err != nil {
		return DomainMutationResult{}, err
	}
	ctx, request, shared, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return DomainMutationResult{}, scopeErr
	}
	if shared {
		return DomainMutationResult{}, ErrMachinePublicationDenied
	}
	request, err := normalizeRequestContext(request, s.newID)
	if err != nil {
		return DomainMutationResult{}, err
	}
	opID, auditID, parentAuditID, err := s.mutationIDs()
	if err != nil {
		return DomainMutationResult{}, err
	}
	record := DomainRecord{OperationID: opID, AuditEventID: auditID, ParentAuditEventID: parentAuditID, AccountID: request.Actor.AccountID, TunnelID: tunnelID, DomainID: domainID,
		ExpectedGeneration: input.ExpectedGeneration, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash, ActorID: request.Actor.ActorID, AuditActorID: auditActorIDForActor(request.Actor), ActorType: auditActorType(request.Actor),
		RequestID: request.RequestID, CorrelationID: request.CorrelationID, SourceMachineID: request.Actor.MachineID, Now: s.now().UTC()}
	result, err := s.repository.DeleteResourceDomain(ctx, record)
	if err != nil {
		return DomainMutationResult{}, err
	}
	return DomainMutationResult{Domain: DomainViewFromRow(result.Domain), Operation: previewtunnelapi.OperationView(result.Operation, request.RequestID), Replayed: result.Replayed, Changed: result.Changed}, nil
}

func validateDNSProvider(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "generic", nil
	}
	switch normalized {
	case "generic", "cloudflare", "route53", "google_cloud_dns", "digitalocean", "namecheap":
		return normalized, nil
	default:
		return "", fmt.Errorf("%w: DNS provider is unsupported", ErrInvalidInput)
	}
}

func normalizeDomainCertificateStrategy(value, matchType, protocol string) (string, error) {
	strategy := strings.ToLower(strings.TrimSpace(value))
	if protocol == "tcp" || protocol == "tls" {
		if strategy == "" || strategy == "none" {
			return "none", nil
		}
		return "", fmt.Errorf("%w: public TCP domains do not use HTTP edge certificates", ErrInvalidInput)
	}
	if strategy == "" {
		strategy = "managed"
	}
	// Every HTTP hostname serves its workspace domain's wildcard certificate.
	switch strategy {
	case "managed":
		return strategy, nil
	case "none":
		return "", fmt.Errorf("%w: HTTP domains require managed certificates", ErrInvalidInput)
	default:
		return "", fmt.Errorf("%w: certificate strategy is unsupported", ErrInvalidInput)
	}
}

func (s *ResourceService) ListTunnelLogs(ctx context.Context, request previewtunnelapi.RequestContext, tunnelID, rawCursor string, limit int) (LogPage, error) {
	if err := s.authorize(request, "read", false); err != nil {
		return LogPage{}, err
	}
	ctx, request, _, scopeErr := scopeTunnelManagement(ctx, s.repository, request, tunnelID)
	if scopeErr != nil {
		return LogPage{}, scopeErr
	}
	if err := validateResourceListLimit(limit); err != nil {
		return LogPage{}, err
	}
	after, err := s.cursors.DecodeLog(rawCursor, "tunnel", request.Actor.AccountID, tunnelID)
	if err != nil {
		return LogPage{}, err
	}
	rows, err := s.repository.ListResourceTunnelLogs(ctx, request.Actor.AccountID, tunnelID, after, limit+1)
	if err != nil {
		return LogPage{}, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	page := LogPage{Items: make([]LogEntry, 0, len(rows))}
	for _, row := range rows {
		entry, entryErr := logView(row)
		if entryErr != nil {
			return LogPage{}, entryErr
		}
		page.Items = append(page.Items, entry)
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		sequence := last.CursorSequence.Int64
		page.NextCursor, err = s.cursors.EncodeLog("tunnel", request.Actor.AccountID, tunnelID, sequence)
		if err != nil {
			return LogPage{}, err
		}
	}
	return page, nil
}

func (s *ResourceService) ListPreviewLogs(ctx context.Context, request previewtunnelapi.RequestContext, previewID, rawCursor string, limit int) (LogPage, error) {
	if err := previewtunnelapi.Authorize(request.Actor, previewtunnelapi.AccessRequest{AccountID: request.Actor.AccountID, Resource: "previews", Action: "read"}); err != nil {
		return LogPage{}, err
	}
	if err := validateResourceListLimit(limit); err != nil {
		return LogPage{}, err
	}
	after, err := s.cursors.DecodeLog(rawCursor, "preview", request.Actor.AccountID, previewID)
	if err != nil {
		return LogPage{}, err
	}
	rows, err := s.repository.ListResourcePreviewLogs(ctx, request.Actor.AccountID, previewID, after, limit+1)
	if err != nil {
		return LogPage{}, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	page := LogPage{Items: make([]LogEntry, 0, len(rows))}
	for _, row := range rows {
		entry, entryErr := logView(row)
		if entryErr != nil {
			return LogPage{}, entryErr
		}
		page.Items = append(page.Items, entry)
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		if !last.CursorSequence.Valid {
			return LogPage{}, fmt.Errorf("invalid log cursor sequence")
		}
		page.NextCursor, err = s.cursors.EncodeLog("preview", request.Actor.AccountID, previewID, last.CursorSequence.Int64)
		if err != nil {
			return LogPage{}, err
		}
	}
	return page, nil
}

func validateResourceListLimit(limit int) error {
	if limit < 1 || limit > previewtunnelapi.MaximumPageLimit {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidInput, previewtunnelapi.MaximumPageLimit)
	}
	return nil
}

func (s *ResourceService) mutationIDs() (string, string, string, error) {
	op, err := s.newID("operation")
	if err != nil {
		return "", "", "", err
	}
	audit, err := s.newID("event")
	if err != nil {
		return "", "", "", err
	}
	parent, err := s.newID("event")
	if err != nil {
		return "", "", "", err
	}
	return op, audit, parent, nil
}

func routeView(row dbsqlc.TunnelRoute) RouteView {
	hostMatch := RouteHostMatch{Type: wireMatchType(row.MatchType), Hostname: nullStringValue(row.MatchHostname)}
	if row.MatchType == "one_label_wildcard" {
		hostMatch.Hostname = "*." + nullStringValue(row.WildcardSuffix)
		labels := 1
		hostMatch.WildcardLabels = &labels
	}
	origin := RouteOrigin{Scheme: row.OriginScheme, Address: row.OriginAddress, PreserveHost: row.PreserveHost, HostOverride: nullStringPointer(row.HostOverride)}
	if row.OriginScheme == "https" {
		origin.TLS = &RouteTLS{Verification: row.TlsVerification, ServerName: nullStringPointer(row.TlsServerName), CAReference: nullStringPointer(row.CaReference), ClientCredentialReference: nullStringPointer(row.MtlsCredentialReference), Pin: nullStringPointer(row.TlsPin)}
	}
	return RouteView{Schema: Schema, Kind: "route", ID: row.ID, TunnelID: row.TunnelID, Name: row.Name, Protocol: wireProtocol(row.Protocol), HostMatch: hostMatch,
		PathPrefix: nullStringPointer(row.PathPrefix), Origin: origin, Priority: row.Priority, ConnectTimeoutMS: row.ConnectTimeoutMs, IdleTimeoutMS: row.IdleTimeoutMs,
		MaxConcurrentStreams: row.MaxConcurrentStreams, DesiredState: row.DesiredState, Generation: row.Generation,
		ETag: previewtunnelapi.ETag("route", row.ID, row.Generation), PublicTCPListenerID: nullStringValue(row.PublicTcpListenerID), PublicTCPPort: row.PublicTcpPort.Int32,
		RewriteRules: routeRewriteRules(row.RewriteRules)}
}

// routeRewriteRules is the stored rules for a view, nil for none (or for a document that no longer parses).
func routeRewriteRules(raw []byte) *tunnelrouterules.Rules {
	rules, err := tunnelrouterules.Decode(raw)
	if err != nil || rules.Empty() {
		return nil
	}
	return &rules
}

// DomainViewFromRow is the wire view of a tunnel domain binding.
func DomainViewFromRow(row dbsqlc.TunnelDomain) DomainView {
	observed := make([]string, 0)
	var raw []any
	if json.Unmarshal(row.ObservedRecords, &raw) == nil {
		for _, value := range raw {
			if text, ok := value.(string); ok {
				observed = append(observed, text)
			}
		}
	}
	dns := DomainDNS{Target: row.DnsTarget, ObservedRecords: observed}
	if row.DnsLastCheckedAt.Valid {
		value := row.DnsLastCheckedAt.Time.UTC()
		dns.LastCheckedAt = &value
	}
	certificate := DomainCertificate{State: wireCertificateState(row.CertificateState)}
	if row.CertificateReference.Valid {
		certificate.Reference = row.CertificateReference.String
	}
	if row.CertificateExpiresAt.Valid {
		value := row.CertificateExpiresAt.Time.UTC()
		certificate.ExpiresAt = &value
	}
	if row.CertificateFailureCode.Valid {
		certificate.Failure = map[string]any{"code": row.CertificateFailureCode.String}
	}
	labels := (*int)(nil)
	if row.MatchType == "one_label_wildcard" {
		value := 1
		labels = &value
	}
	return DomainView{Schema: Schema, Kind: "domain_binding", ID: row.ID, AccountID: row.AccountID, TunnelID: row.TunnelID, RouteID: row.RouteID, Hostname: row.Hostname, MatchType: row.MatchType,
		WildcardLabels: labels, CertificateStrategy: row.CertificateStrategy, State: wireDomainLifecycleState(row), DNS: dns, Certificate: certificate, Generation: row.Generation, ETag: previewtunnelapi.ETag("domain_binding", row.ID, row.Generation)}
}

func wireDomainLifecycleState(row dbsqlc.TunnelDomain) string {
	dnsState := wireDomainState(row.OwnershipState, row.ConflictState)
	if dnsState != "verified" {
		return dnsState
	}
	switch row.CertificateState {
	case "pending", "issuing", "renewing":
		return "issuing_tls"
	case "ready":
		return "ready"
	case "failed", "expired", "revoked":
		return "tls_error"
	default:
		return "verified"
	}
}

func logView(row any) (LogEntry, error) {
	var entry dbsqlc.TunnelLogEntry
	switch value := row.(type) {
	case dbsqlc.TunnelLogEntry:
		entry = value
	case dbsqlc.ListTunnelLogsV1Row:
		entry = dbsqlc.TunnelLogEntry{ID: value.ID, AccountID: value.AccountID, TunnelID: value.TunnelID, PreviewID: value.PreviewID, RouteID: value.RouteID, ConnectorID: value.ConnectorID, SessionID: value.SessionID,
			Level: value.Level, Component: value.Component, Code: value.Code, Message: value.Message, Metadata: value.Metadata, CorrelationID: value.CorrelationID, OccurredAt: value.OccurredAt, CursorSequence: value.CursorSequence}
	default:
		return LogEntry{}, fmt.Errorf("invalid log row type")
	}
	if !entry.CursorSequence.Valid {
		return LogEntry{}, fmt.Errorf("invalid log cursor sequence")
	}
	if err := validateLogText(entry.Message, maxResourceLogMessageBytes); err != nil {
		return LogEntry{}, fmt.Errorf("unsafe log message: %w", err)
	}
	if err := validateLogCorrelation(entry.CorrelationID); err != nil {
		return LogEntry{}, fmt.Errorf("unsafe log correlation: %w", err)
	}
	var metadata map[string]any
	decoder := json.NewDecoder(bytes.NewReader(entry.Metadata))
	decoder.UseNumber()
	if err := decoder.Decode(&metadata); err != nil {
		return LogEntry{}, fmt.Errorf("decode log metadata: %w", err)
	}
	safe, err := previewtunnelapi.SafeMetadata(metadata)
	if err != nil {
		return LogEntry{}, err
	}
	return LogEntry{Schema: Schema, Kind: "log_entry", ID: entry.ID, TunnelID: nullStringValue(entry.TunnelID), PreviewID: nullStringValue(entry.PreviewID), RouteID: nullStringValue(entry.RouteID), ConnectorID: nullStringValue(entry.ConnectorID), SessionID: nullStringValue(entry.SessionID),
		Level: entry.Level, Component: entry.Component, Code: entry.Code, Message: entry.Message, Metadata: safe, CorrelationID: entry.CorrelationID, OccurredAt: entry.OccurredAt.UTC(), Cursor: strconv.FormatInt(entry.CursorSequence.Int64, 10)}, nil
}

func validateLogText(value string, maxLength int) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxLength || strings.ContainsAny(value, "\r\n\x00") {
		return previewtunnelapi.ErrUnsafeMetadata
	}
	upper := strings.ToUpper(value)
	if strings.Contains(upper, "BEGIN PRIVATE KEY") || strings.Contains(upper, "BEARER ") || strings.Contains(upper, "PASSWORD=") || strings.Contains(upper, "TOKEN=") || strings.Contains(upper, "AUTHORIZATION=") || strings.Contains(upper, "AUTHORIZATION:") {
		return previewtunnelapi.ErrUnsafeMetadata
	}
	for _, token := range strings.Fields(value) {
		candidate := strings.Trim(token, "\"'()[]{}<>,.;")
		if !strings.Contains(candidate, "://") {
			continue
		}
		parsed, err := url.Parse(candidate)
		if err != nil || !parsed.IsAbs() {
			continue
		}
		if parsed.User != nil {
			return previewtunnelapi.ErrUnsafeMetadata
		}
		for key := range parsed.Query() {
			normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
			if strings.Contains(normalized, "token") || strings.Contains(normalized, "secret") || normalized == "password" || normalized == "authorization" {
				return previewtunnelapi.ErrUnsafeMetadata
			}
		}
	}
	return nil
}

func validateLogCorrelation(value string) error {
	if err := validateLogText(value, 256); err != nil {
		return err
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("._:-", character)) {
			return previewtunnelapi.ErrUnsafeMetadata
		}
	}
	return nil
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}

type normalizedRouteCreate struct {
	Name                 string
	Protocol             string
	MatchType            string
	Hostname             sql.NullString
	WildcardSuffix       sql.NullString
	PathPrefix           sql.NullString
	Origin               RouteOriginRequest
	Priority             int32
	ConnectTimeoutMS     int32
	IdleTimeoutMS        int32
	MaxConcurrentStreams int32
	RewriteRules         []byte
}

type normalizedRoutePatch struct {
	Name                 string
	Protocol             string
	MatchType            string
	Hostname             sql.NullString
	WildcardSuffix       sql.NullString
	PathPrefix           sql.NullString
	Origin               RouteOriginRequest
	Priority             int32
	ConnectTimeoutMS     int32
	IdleTimeoutMS        int32
	MaxConcurrentStreams int32
	DesiredState         string
	RewriteRules         []byte
}

// normalizeRewriteRules is the canonical JSON of a route's rules ("{}" for none). Rules apply to http routes only.
func normalizeRewriteRules(rules *tunnelrouterules.Rules, protocol string) ([]byte, error) {
	if rules == nil || rules.Empty() {
		return []byte("{}"), nil
	}
	if protocol != "" && protocol != "http" {
		return nil, fmt.Errorf("%w: rewrite rules apply to http routes only", ErrInvalidInput)
	}
	canonical, err := tunnelrouterules.Canonical(*rules)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return canonical, nil
}

func normalizeRouteCreate(input RouteCreateRequest) (normalizedRouteCreate, error) {
	name, err := validateRouteName(input.Name)
	if err != nil {
		return normalizedRouteCreate{}, err
	}
	protocol, err := normalizeWireProtocol(input.Protocol)
	if err != nil {
		return normalizedRouteCreate{}, err
	}
	matchType, hostname, wildcard, err := normalizeRouteMatch(input.MatchType, input.Hostname, input.WildcardSuffix)
	if err != nil {
		return normalizedRouteCreate{}, err
	}
	origin, err := normalizeRouteOrigin(input.Origin)
	if err != nil {
		return normalizedRouteCreate{}, err
	}
	pathPrefix, err := normalizePathPrefix(input.PathPrefix)
	if err != nil {
		return normalizedRouteCreate{}, err
	}
	if protocol == "tcp_private" && origin.Scheme != "tcp" {
		return normalizedRouteCreate{}, fmt.Errorf("%w: tcp_private requires a tcp origin", ErrInvalidInput)
	}
	if protocol == "tcp_private" && (matchType != "catch_all" || hostname.Valid || wildcard.Valid || pathPrefix.Valid) {
		return normalizedRouteCreate{}, fmt.Errorf("%w: tcp_private requires a catch-all host match without a path prefix", ErrInvalidInput)
	}
	if (protocol == "tcp" || protocol == "tls") && (origin.Scheme != "tcp" || matchType != "managed" || !hostname.Valid || wildcard.Valid || pathPrefix.Valid || origin.PreserveHost || origin.HostOverride != nil || origin.TLS != nil) {
		return normalizedRouteCreate{}, fmt.Errorf("%w: public tcp/tls requires an exact managed hostname, plain tcp origin, and no HTTP/TLS-origin settings", ErrInvalidInput)
	}
	if protocol == "http" && origin.Scheme == "tcp" {
		return normalizedRouteCreate{}, fmt.Errorf("%w: HTTP protocol cannot target a tcp origin", ErrInvalidInput)
	}
	if input.Priority == 0 {
		input.Priority = 100
	}
	if input.Priority < 0 || input.Priority > 1_000_000 {
		return normalizedRouteCreate{}, fmt.Errorf("%w: priority is out of range", ErrInvalidInput)
	}
	connectTimeout, idleTimeout, maxStreams, err := normalizeRouteLimits(input.ConnectTimeoutMS, input.IdleTimeoutMS, input.MaxConcurrentStreams)
	if err != nil {
		return normalizedRouteCreate{}, err
	}
	rewriteRules, err := normalizeRewriteRules(input.RewriteRules, protocol)
	if err != nil {
		return normalizedRouteCreate{}, err
	}
	return normalizedRouteCreate{Name: name, Protocol: protocol, MatchType: matchType, Hostname: hostname, WildcardSuffix: wildcard, PathPrefix: pathPrefix, Origin: origin, Priority: input.Priority,
		ConnectTimeoutMS: connectTimeout, IdleTimeoutMS: idleTimeout, MaxConcurrentStreams: maxStreams, RewriteRules: rewriteRules}, nil
}

func normalizeRoutePatch(input RoutePatchRequest) (normalizedRoutePatch, error) {
	result := normalizedRoutePatch{}
	if input.Name != nil {
		value, err := validateRouteName(*input.Name)
		if err != nil {
			return result, err
		}
		result.Name = value
	}
	if input.Protocol != nil {
		value, err := normalizeWireProtocol(*input.Protocol)
		if err != nil {
			return result, err
		}
		result.Protocol = value
	}
	if input.MatchType != nil || input.Hostname != nil || input.WildcardSuffix != nil {
		matchType := ""
		if input.MatchType != nil {
			matchType = *input.MatchType
		}
		hostname := ""
		if input.Hostname != nil {
			hostname = *input.Hostname
		}
		wildcard := ""
		if input.WildcardSuffix != nil {
			wildcard = *input.WildcardSuffix
		}
		value, host, suffix, err := normalizeRouteMatch(matchType, hostname, wildcard)
		if err != nil {
			return result, err
		}
		result.MatchType, result.Hostname, result.WildcardSuffix = value, host, suffix
	}
	if input.PathPrefixSet {
		value, err := normalizePathPrefix(input.PathPrefix)
		if err != nil {
			return result, err
		}
		result.PathPrefix = value
	}
	if input.Origin != nil {
		value, err := normalizeRouteOrigin(*input.Origin)
		if err != nil {
			return result, err
		}
		result.Origin = value
	}
	if input.Priority != nil {
		if *input.Priority < 0 || *input.Priority > 1_000_000 {
			return result, fmt.Errorf("%w: priority is out of range", ErrInvalidInput)
		}
		result.Priority = *input.Priority
	}
	if input.ConnectTimeoutMS != nil || input.IdleTimeoutMS != nil || input.MaxConcurrentStreams != nil {
		connectTimeout, idleTimeout, maxStreams, err := normalizeRouteLimits(valueOrZero(input.ConnectTimeoutMS), valueOrZero(input.IdleTimeoutMS), valueOrZero(input.MaxConcurrentStreams))
		if err != nil {
			return result, err
		}
		result.ConnectTimeoutMS, result.IdleTimeoutMS, result.MaxConcurrentStreams = connectTimeout, idleTimeout, maxStreams
	}
	if input.RewriteRulesSet {
		// the route's protocol is checked against the stored one with the candidate (validateEffectiveRouteCandidate)
		value, err := normalizeRewriteRules(input.RewriteRules, "")
		if err != nil {
			return result, err
		}
		result.RewriteRules = value
	}
	if input.DesiredState != nil {
		switch *input.DesiredState {
		case "active", "disabled":
			result.DesiredState = *input.DesiredState
		default:
			return result, fmt.Errorf("%w: desired_state is invalid", ErrInvalidInput)
		}
	}
	return result, nil
}

func valueOrZero(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

// DefaultRouteStreams is a route's concurrent stream bound when the owner sets none. It protects
// the owner's origin, not the edge, so it is high enough for a popular page (edge abuse plan, section 5).
const DefaultRouteStreams = 1024

func normalizeRouteLimits(connectTimeout, idleTimeout, maxStreams int32) (int32, int32, int32, error) {
	if connectTimeout == 0 {
		connectTimeout = 10_000
	}
	if idleTimeout == 0 {
		idleTimeout = 90_000
	}
	if maxStreams == 0 {
		maxStreams = DefaultRouteStreams
	}
	if connectTimeout < 100 || connectTimeout > 120_000 || idleTimeout < 1_000 || idleTimeout > 3_600_000 || maxStreams < 1 || maxStreams > 10_000 {
		return 0, 0, 0, fmt.Errorf("%w: route timeout or stream limit is out of range", ErrInvalidInput)
	}
	return connectTimeout, idleTimeout, maxStreams, nil
}

func validateRouteName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 1 || len(value) > 80 || !tunnelNamePattern.MatchString(value) {
		return "", fmt.Errorf("%w: route name is invalid", ErrInvalidInput)
	}
	return value, nil
}

func normalizeWireProtocol(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "http":
		return "http", nil
	case "tcp_private":
		return "tcp_private", nil
	case "tcp":
		return "tcp", nil
	case "tls":
		return "tls", nil
	default:
		return "", fmt.Errorf("%w: protocol must be http, tcp, tls, or tcp_private", ErrInvalidInput)
	}
}

func dbProtocol(value string) string {
	if value == "tcp_private" {
		return "private_tcp"
	}
	return value
}
func wireProtocol(value string) string {
	if value == "private_tcp" {
		return "tcp_private"
	}
	return value
}

func normalizeRouteMatch(matchType, hostname, wildcardSuffix string) (string, sql.NullString, sql.NullString, error) {
	matchType = strings.ToLower(strings.TrimSpace(matchType))
	if matchType == "managed_exact" {
		matchType = "managed"
	}
	switch matchType {
	case "managed", "exact":
		if hostname == "" {
			return "", sql.NullString{}, sql.NullString{}, fmt.Errorf("%w: hostname is required", ErrInvalidInput)
		}
		if strings.HasPrefix(hostname, "*.") || wildcardSuffix != "" {
			return "", sql.NullString{}, sql.NullString{}, fmt.Errorf("%w: wildcard is only valid for one_label_wildcard", ErrInvalidInput)
		}
		normalized, err := normalizeIDNAHostname(hostname)
		if err != nil {
			return "", sql.NullString{}, sql.NullString{}, fmt.Errorf("%w: hostname: %v", ErrInvalidInput, err)
		}
		return matchType, nullableString(normalized), sql.NullString{}, nil
	case "one_label_wildcard":
		if wildcardSuffix == "" && strings.HasPrefix(hostname, "*.") {
			wildcardSuffix = strings.TrimPrefix(hostname, "*.")
		}
		if strings.HasPrefix(wildcardSuffix, "*.") || wildcardSuffix == "" {
			return "", sql.NullString{}, sql.NullString{}, fmt.Errorf("%w: one-label wildcard suffix is required", ErrInvalidInput)
		}
		normalized, err := normalizeIDNAHostname(wildcardSuffix)
		if err != nil {
			return "", sql.NullString{}, sql.NullString{}, fmt.Errorf("%w: wildcard suffix: %v", ErrInvalidInput, err)
		}
		if len(strings.Split(normalized, ".")) < 2 {
			return "", sql.NullString{}, sql.NullString{}, fmt.Errorf("%w: wildcard must contain a registrable suffix", ErrInvalidInput)
		}
		return matchType, sql.NullString{}, nullableString(normalized), nil
	case "catch_all":
		if hostname != "" || wildcardSuffix != "" {
			return "", sql.NullString{}, sql.NullString{}, fmt.Errorf("%w: catch-all cannot have a hostname", ErrInvalidInput)
		}
		return matchType, sql.NullString{}, sql.NullString{}, nil
	default:
		return "", sql.NullString{}, sql.NullString{}, fmt.Errorf("%w: host match type is invalid", ErrInvalidInput)
	}
}

func normalizePathPrefix(value *string) (sql.NullString, error) {
	if value == nil {
		return sql.NullString{}, nil
	}
	if *value == "" {
		return sql.NullString{}, nil
	}
	if len(*value) > 2048 || !strings.HasPrefix(*value, "/") || strings.ContainsAny(*value, "\r\n\x00") {
		return sql.NullString{}, fmt.Errorf("%w: path_prefix is invalid", ErrInvalidInput)
	}
	return nullableString(*value), nil
}

func normalizeRouteOrigin(input RouteOriginRequest) (RouteOriginRequest, error) {
	input.Scheme = strings.ToLower(strings.TrimSpace(input.Scheme))
	input.Address = strings.TrimSpace(input.Address)
	if input.HostOverride != nil {
		value := strings.ToLower(strings.TrimSpace(*input.HostOverride))
		input.HostOverride = &value
	}
	if err := validateOriginRequest(OriginRequest{Scheme: input.Scheme, Address: input.Address, PreserveHost: &input.PreserveHost, HostOverride: input.HostOverride}); err != nil {
		return RouteOriginRequest{}, err
	}
	if input.Scheme != "https" && input.TLS != nil {
		return RouteOriginRequest{}, fmt.Errorf("%w: TLS settings require an HTTPS origin", ErrInvalidInput)
	}
	if input.Scheme == "https" {
		if input.TLS == nil {
			input.TLS = &RouteTLSRequest{Verification: "system"}
		}
		if err := validateRouteTLS(*input.TLS); err != nil {
			return RouteOriginRequest{}, err
		}
	} else {
		input.TLS = nil
	}
	return input, nil
}

// tlsPinPattern is a pinned route's certificate fingerprint (SHA-256 of the leaf certificate DER).
var tlsPinPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func validateRouteTLS(tls RouteTLSRequest) error {
	switch tls.Verification {
	case "system", "custom_ca", "insecure", "pinned":
	default:
		return fmt.Errorf("%w: TLS verification is invalid", ErrInvalidInput)
	}
	if (tls.Verification == "pinned") != (tls.Pin != nil) || tls.Pin != nil && !tlsPinPattern.MatchString(*tls.Pin) {
		return fmt.Errorf("%w: pinned verification needs pin sha256:<64 lowercase hex>, and only pinned takes a pin", ErrInvalidInput)
	}
	if tls.Verification == "custom_ca" && (tls.CAReference == nil || strings.TrimSpace(*tls.CAReference) == "") {
		return fmt.Errorf("%w: custom CA reference is required", ErrInvalidInput)
	}
	if tls.Verification != "custom_ca" && tls.CAReference != nil {
		return fmt.Errorf("%w: CA reference requires custom_ca verification", ErrInvalidInput)
	}
	for name, value := range map[string]*string{"server_name": tls.ServerName, "ca_reference": tls.CAReference, "client_credential_reference": tls.ClientCredentialReference} {
		if value != nil {
			if err := validateWriteOnlyReferenceOrHost(name, *value); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateWriteOnlyReferenceOrHost(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\r\n\t") {
		return fmt.Errorf("%w: %s is invalid", ErrInvalidInput, name)
	}
	if name != "server_name" {
		return validateWriteOnlyReference(value)
	}
	if err := validateHostOnly(value); err != nil {
		return fmt.Errorf("%w: TLS server name is invalid", ErrInvalidInput)
	}
	return nil
}

func validateWriteOnlyReference(value string) error {
	value = strings.TrimSpace(value)
	if len(value) < 24 || len(value) > 512 || strings.ContainsAny(value, "\r\n\t ") || strings.Contains(strings.ToLower(value), "bearer") || strings.Contains(strings.ToLower(value), "token=") {
		return fmt.Errorf("%w: write-only reference is invalid", ErrInvalidInput)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.Host != "paperboat" || parsed.RawQuery != "" || parsed.Fragment != "" || len(parsed.Path) < 2 {
		return fmt.Errorf("%w: write-only reference must be an opaque Paperboat secret-store reference", ErrInvalidInput)
	}
	switch parsed.Scheme {
	case "keychain", "credential-manager", "secret-service", "protected-file", "tpm":
	default:
		return fmt.Errorf("%w: write-only reference scheme is invalid", ErrInvalidInput)
	}
	for _, character := range strings.TrimPrefix(parsed.Path, "/") {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("._~:/@+-", character) {
			continue
		}
		return fmt.Errorf("%w: write-only reference contains invalid characters", ErrInvalidInput)
	}
	return nil
}

func normalizeBindingHostname(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, ".")
	if value == "" || strings.HasSuffix(value, ".") {
		return "", "", fmt.Errorf("%w: hostname is invalid", ErrInvalidInput)
	}
	if strings.HasPrefix(value, "*.") {
		suffix, err := normalizeIDNAHostname(strings.TrimPrefix(value, "*."))
		if err != nil {
			return "", "", fmt.Errorf("%w: hostname: %v", ErrInvalidInput, err)
		}
		if len(strings.Split(suffix, ".")) < 2 {
			return "", "", fmt.Errorf("%w: wildcard must contain a registrable suffix", ErrInvalidInput)
		}
		return "*." + suffix, "one_label_wildcard", nil
	}
	if strings.Contains(value, "*") {
		return "", "", fmt.Errorf("%w: wildcard must cover exactly one label", ErrInvalidInput)
	}
	value, err := normalizeIDNAHostname(value)
	if err != nil {
		return "", "", fmt.Errorf("%w: hostname: %v", ErrInvalidInput, err)
	}
	return value, "exact", nil
}

func normalizeIDNAHostname(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n\t /?#@") {
		return "", errors.New("DNS name is invalid")
	}
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil {
		return "", errors.New("DNS name is invalid")
	}
	ascii = strings.ToLower(ascii)
	if err := validateDNSName(ascii); err != nil {
		return "", err
	}
	return ascii, nil
}

func wireMatchType(value string) string {
	if value == "managed" {
		return "managed_exact"
	}
	return value
}

func wireDomainState(ownership, conflict string) string {
	if conflict == "quarantined" {
		return "quarantined"
	}
	if conflict == "conflicted" {
		return "conflict"
	}
	switch ownership {
	case "pending":
		return "waiting_dns"
	case "verified":
		return "verified"
	case "failed":
		return "dns_error"
	case "expired":
		return "expired"
	case "revoked":
		return "quarantined"
	default:
		return "requested"
	}
}

func wireCertificateState(value string) string {
	if value == "pending" {
		return "not_requested"
	}
	return value
}

type resourceCursorCodec struct{ key []byte }
type resourceCursorPayload struct {
	Version   int       `json:"version"`
	Kind      string    `json:"kind"`
	AccountID string    `json:"account_id"`
	TunnelID  string    `json:"tunnel_id"`
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}
type logCursorPayload struct {
	Version   int    `json:"version"`
	Kind      string `json:"kind"`
	AccountID string `json:"account_id"`
	ScopeID   string `json:"scope_id"`
	Sequence  int64  `json:"sequence"`
}

func newResourceCursorCodec(key []byte) (*resourceCursorCodec, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("resource cursor signing key must contain at least 32 bytes")
	}
	return &resourceCursorCodec{key: append([]byte(nil), key...)}, nil
}
func (c *resourceCursorCodec) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, c.key)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}
func (c *resourceCursorCodec) encode(payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > 512 {
		return "", previewtunnelapi.ErrInvalidCursor
	}
	signature := c.sign(raw)
	token := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if len(token) > 2048 {
		return "", previewtunnelapi.ErrInvalidCursor
	}
	return token, nil
}
func (c *resourceCursorCodec) decode(raw string, target any) error {
	if raw == "" {
		return nil
	}
	if len(raw) > 2048 {
		return previewtunnelapi.ErrInvalidCursor
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return previewtunnelapi.ErrInvalidCursor
	}
	enc := base64.RawURLEncoding.Strict()
	payload, err := enc.DecodeString(parts[0])
	if err != nil || len(payload) == 0 || len(payload) > 512 {
		return previewtunnelapi.ErrInvalidCursor
	}
	signature, err := enc.DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size || !hmac.Equal(signature, c.sign(payload)) {
		return previewtunnelapi.ErrInvalidCursor
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return previewtunnelapi.ErrInvalidCursor
	}
	return nil
}
func (c *resourceCursorCodec) EncodeList(kind, accountID, tunnelID string, position ListPosition) (string, error) {
	if kind == "" || accountID == "" || tunnelID == "" || position.ID == "" || position.CreatedAt.IsZero() {
		return "", previewtunnelapi.ErrInvalidCursor
	}
	return c.encode(resourceCursorPayload{Version: 1, Kind: kind, AccountID: accountID, TunnelID: tunnelID, CreatedAt: position.CreatedAt.UTC(), ID: position.ID})
}
func (c *resourceCursorCodec) DecodeList(raw, kind, accountID, tunnelID string) (*ListPosition, error) {
	if raw == "" {
		return nil, nil
	}
	var value resourceCursorPayload
	if err := c.decode(raw, &value); err != nil || value.Version != 1 || value.Kind != kind || value.AccountID != accountID || value.TunnelID != tunnelID || value.ID == "" || value.CreatedAt.IsZero() {
		return nil, previewtunnelapi.ErrInvalidCursor
	}
	return &ListPosition{ID: value.ID, CreatedAt: value.CreatedAt.UTC()}, nil
}
func (s *ResourceService) decodeListCursor(raw, accountID, tunnelID, kind string) (*ListPosition, error) {
	return s.cursors.DecodeList(raw, kind, accountID, tunnelID)
}
func (c *resourceCursorCodec) EncodeLog(kind, accountID, scopeID string, sequence int64) (string, error) {
	if kind == "" || accountID == "" || scopeID == "" || sequence < 0 {
		return "", previewtunnelapi.ErrInvalidCursor
	}
	return c.encode(logCursorPayload{Version: 1, Kind: kind, AccountID: accountID, ScopeID: scopeID, Sequence: sequence})
}
func (c *resourceCursorCodec) DecodeLog(raw, kind, accountID, scopeID string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	var value logCursorPayload
	if err := c.decode(raw, &value); err != nil || value.Version != 1 || value.Kind != kind || value.AccountID != accountID || value.ScopeID != scopeID || value.Sequence < 0 {
		return 0, previewtunnelapi.ErrInvalidCursor
	}
	return value.Sequence, nil
}
