package tunnelv1

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"github.com/pinksaucepasta/paperboat/shared/tunnelv1"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
)

// ResourceConfig controls the resource service's deterministic dependencies.
// CursorKey is deliberately separate from the tunnel list key so a token from
// one resource family can never be replayed against another family.
type ResourceConfig struct {
	CursorKey        []byte
	Now              func() time.Time
	NewID            func(prefix string) (string, error)
	PublicTCPPortMin int32
	PublicTCPPortMax int32
}

type ResourceAPI interface {
	ListRoutes(context.Context, previewtunnelapi.RequestContext, string, string, int) (RoutePage, error)
	GetRoute(context.Context, previewtunnelapi.RequestContext, string, string) (RouteView, error)
	CreateRoute(context.Context, previewtunnelapi.RequestContext, string, RouteCreateRequest) (RouteMutationResult, error)
	PatchRoute(context.Context, previewtunnelapi.RequestContext, string, string, RoutePatchRequest) (RouteMutationResult, error)
	DeleteRoute(context.Context, previewtunnelapi.RequestContext, string, string, ResourceMutationInput) (RouteMutationResult, error)

	ListDomains(context.Context, previewtunnelapi.RequestContext, string, string, int) (DomainPage, error)
	GetDomain(context.Context, previewtunnelapi.RequestContext, string, string) (DomainView, error)
	CreateDomain(context.Context, previewtunnelapi.RequestContext, string, DomainCreateRequest) (DomainMutationResult, error)
	DeleteDomain(context.Context, previewtunnelapi.RequestContext, string, string, ResourceMutationInput) (DomainMutationResult, error)

	ListTunnelLogs(context.Context, previewtunnelapi.RequestContext, string, string, int) (LogPage, error)
	ListPreviewLogs(context.Context, previewtunnelapi.RequestContext, string, string, int) (LogPage, error)
}

// PreviewLogAPI is kept independent from the lifecycle PreviewTunnelAPI so a
// browser cannot accidentally gain access to enrollment or write-only data.
type PreviewLogAPI interface {
	ListPreviewLogs(context.Context, previewtunnelapi.RequestContext, string, string, int) (LogPage, error)
}

type ResourceMutationInput struct {
	ExpectedGeneration int64
	IdempotencyKey     string
	RequestHash        [sha256.Size]byte
}

type RouteCreateRequest struct {
	Name                 string
	Protocol             string
	MatchType            string
	Hostname             string
	WildcardSuffix       string
	PathPrefix           *string
	Origin               RouteOriginRequest
	Priority             int32
	ConnectTimeoutMS     int32
	IdleTimeoutMS        int32
	MaxConcurrentStreams int32
	// RewriteRules are the route's header, path and redirect rules (protocol http only).
	RewriteRules *tunnelrouterules.Rules
	Mutation     ResourceMutationInput
}

type RoutePatchRequest struct {
	Name                 *string
	Protocol             *string
	MatchType            *string
	Hostname             *string
	WildcardSuffix       *string
	PathPrefix           *string
	PathPrefixSet        bool
	Origin               *RouteOriginRequest
	Priority             *int32
	ConnectTimeoutMS     *int32
	IdleTimeoutMS        *int32
	MaxConcurrentStreams *int32
	DesiredState         *string
	// RewriteRules replaces the route's rules when RewriteRulesSet; a nil or empty value clears them.
	RewriteRules    *tunnelrouterules.Rules
	RewriteRulesSet bool
	Mutation        ResourceMutationInput
}

type RouteTLSRequest struct {
	Verification string
	ServerName   *string
	// CA is the PEM CA a custom_ca route trusts for its origin (stored with the route).
	CA                        *string
	ClientCredentialReference *string
	// Pin is "sha256:<hex>" of the one certificate a pinned route trusts.
	Pin *string
}

type RouteOriginRequest struct {
	Scheme       string
	Address      string
	PreserveHost bool
	HostOverride *string
	TLS          *RouteTLSRequest
}

type DomainCreateRequest struct {
	Hostname            string
	RouteID             string
	Provider            string
	CertificateStrategy string
	Mutation            ResourceMutationInput
}

type RouteHostMatch = tunnelv1.RouteHostMatch

type RouteTLS = tunnelv1.RouteTLS

type RouteOrigin = tunnelv1.RouteOrigin

type RouteView = tunnelv1.Route

type RoutePage = tunnelv1.RoutePage

type DomainDNS = tunnelv1.DomainDNS

type DomainCertificate = tunnelv1.DomainCertificate

type DomainView = tunnelv1.Domain

type DomainPage = tunnelv1.DomainPage

type RouteMutationResult struct {
	Route     RouteView                  `json:"route"`
	Operation previewtunnelapi.Operation `json:"operation"`
	Replayed  bool                       `json:"replayed"`
	Changed   bool                       `json:"changed"`
}

type DomainMutationResult struct {
	Domain    DomainView                 `json:"domain"`
	Operation previewtunnelapi.Operation `json:"operation"`
	Replayed  bool                       `json:"replayed"`
	Changed   bool                       `json:"changed"`
}

type LogEntry = tunnelv1.LogEntry

type LogPage = tunnelv1.LogPage

type ResourceMutationRecord struct {
	Route             dbsqlc.TunnelRoute
	Domain            dbsqlc.TunnelDomain
	Operation         dbsqlc.Operation
	StableEndpointID  string
	ProcessGeneration int64
	Replayed          bool
	Changed           bool
}

type ResourceRepository interface {
	ResolveManagementAccount(context.Context, string, string) (string, bool, error)
	VerifyHost(context.Context, string, string) error
	ListResourceRoutes(context.Context, string, string, *ListPosition, int) ([]dbsqlc.TunnelRoute, error)
	GetResourceRoute(context.Context, string, string, string) (dbsqlc.TunnelRoute, error)
	CreateResourceRoute(context.Context, RouteRecord) (ResourceMutationRecord, error)
	PatchResourceRoute(context.Context, RouteRecord) (ResourceMutationRecord, error)
	DeleteResourceRoute(context.Context, RouteRecord) (ResourceMutationRecord, error)
	ListResourceDomains(context.Context, string, string, *ListPosition, int) ([]dbsqlc.TunnelDomain, error)
	GetResourceDomain(context.Context, string, string, string) (dbsqlc.TunnelDomain, error)
	CreateResourceDomain(context.Context, DomainRecord) (ResourceMutationRecord, error)
	DeleteResourceDomain(context.Context, DomainRecord) (ResourceMutationRecord, error)
	ListResourceTunnelLogs(context.Context, string, string, int64, int) ([]dbsqlc.ListTunnelLogsV1Row, error)
	ListResourcePreviewLogs(context.Context, string, string, int64, int) ([]dbsqlc.TunnelLogEntry, error)
}

type RouteRecord struct {
	OperationID          string
	AuditEventID         string
	ParentAuditEventID   string
	AccountID            string
	TunnelID             string
	RouteID              string
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
	ExpectedGeneration   int64
	IdempotencyKey       string
	RequestHash          [sha256.Size]byte
	ActorID              string
	AuditActorID         string
	ActorType            string
	RequestID            string
	CorrelationID        string
	SourceMachineID      string
	Now                  time.Time
	PublicTCPListenerID  string
	PublicTCPPortMin     int32
	PublicTCPPortMax     int32
	NameSet              bool
	ProtocolSet          bool
	MatchTypeSet         bool
	HostnameSet          bool
	WildcardSuffixSet    bool
	PathPrefixSet        bool
	OriginSet            bool
	PrioritySet          bool
	ConnectTimeoutSet    bool
	IdleTimeoutSet       bool
	MaxStreamsSet        bool
	DesiredStateSet      bool
	// RewriteRules is the canonical JSON ("{}" for none); RewriteRulesSet when a patch replaces it.
	RewriteRules    []byte
	RewriteRulesSet bool
}

type DomainRecord struct {
	OperationID         string
	AuditEventID        string
	ParentAuditEventID  string
	AccountID           string
	TunnelID            string
	DomainID            string
	RouteID             string
	Hostname            string
	MatchType           string
	CertificateStrategy string
	ChallengeReference  string
	DNSTarget           string
	DNSProvider         string
	ExpectedRecords     []byte
	ExpectedGeneration  int64
	IdempotencyKey      string
	RequestHash         [sha256.Size]byte
	ActorID             string
	AuditActorID        string
	ActorType           string
	RequestID           string
	CorrelationID       string
	SourceMachineID     string
	Now                 time.Time
}
