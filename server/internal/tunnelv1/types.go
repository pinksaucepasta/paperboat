package tunnelv1

import (
	"context"
	"crypto/sha256"
	"github.com/pinksaucepasta/paperboat/shared/tunnelv1"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
)

// Schema is the public preview/tunnel resource family identifier.
const Schema = "paperboat.preview-tunnel/v1"

const (
	DesiredActive  = "active"
	DesiredPaused  = "paused"
	DesiredDeleted = "deleted"
	AccessPublic   = "public"
	AccessPrivate  = "private"
	AccessTeam     = "team"
)

// OriginRequest is the initial route target supplied when a durable tunnel is
// created. Credentials are intentionally not accepted in this v1 request.
type OriginRequest struct {
	Scheme       string  `json:"scheme"`
	Address      string  `json:"address"`
	PreserveHost *bool   `json:"preserve_host,omitempty"`
	HostOverride *string `json:"host_override,omitempty"`
}

// MutationInput carries the common idempotency and optimistic-concurrency
// inputs for a tunnel mutation. RequestHash is computed from the exact
// canonical request body by the HTTP boundary.
type MutationInput struct {
	ExpectedGeneration int64
	IdempotencyKey     string
	RequestHash        [sha256.Size]byte
}

type CreateTunnelRequest struct {
	Name       string
	AccessMode string
	Origin     OriginRequest
	ExpiresAt  *time.Time
	MutationInput
}

type PatchTunnelRequest struct {
	Name       *string
	AccessMode *string
	ExpiresAt  *time.Time
	// ExpirySet distinguishes an omitted expires_at field from an explicit
	// null, which removes an existing expiry.
	ExpirySet bool
	MutationInput
}

type TunnelView = tunnelv1.Tunnel

type MutationResult struct {
	Tunnel    TunnelView                 `json:"tunnel"`
	Operation previewtunnelapi.Operation `json:"operation"`
	Replayed  bool                       `json:"replayed"`
	Changed   bool                       `json:"changed"`
}

type TunnelPage = tunnelv1.TunnelPage

type HealthDimension = tunnelv1.HealthDimension

type HealthDimensions = tunnelv1.HealthDimensions

type HealthView = tunnelv1.Health

type FloodProtection = tunnelv1.FloodProtection

// EndpointBuilder allocates a user-facing stable endpoint from the immutable
// endpoint identity. The name argument is retained at this boundary so callers
// do not need a coordinated interface change, but endpoint implementations must
// not derive public hostnames from it.
type EndpointBuilder func(name, stableEndpointID string) (string, error)

type Config struct {
	EndpointBuilder EndpointBuilder
	CursorKey       []byte
	Now             func() time.Time
	NewID           func(prefix string) (string, error)
	// NewEndpointID allocates the opaque UUID used as the leftmost managed
	// tunnel hostname label. It is separate from internal resource IDs so
	// changing the tunnel name can never change the public endpoint.
	NewEndpointID    func() (string, error)
	PublicTCPPortMin int32
	PublicTCPPortMax int32
	// BandwidthDenied, when set, reports whether the account's workspaces are out of bandwidth
	// allowance so status can say so. It must fail open.
	BandwidthDenied func(ctx context.Context, accountID string, at time.Time) bool
}

type ExpiryReconcileRequest struct {
	Now           time.Time
	Limit         int
	ActorID       string
	ActorType     string
	RequestID     string
	CorrelationID string
}
