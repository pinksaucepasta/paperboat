// Package privateaccess contains the server authority for private preview and
// tunnel requests.  It deliberately returns a short-lived decision, never a
// reusable bearer or connector credential.
package privateaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const (
	Schema = connectorprotocol.PrivateAccessSchema
	Kind   = connectorprotocol.PrivateAccessGrantKind

	ResourcePreview = "preview"
	ResourceTunnel  = "tunnel"

	ProtocolHTTP = "http"
	ProtocolTCP  = "tcp"

	AudiencePreviewHTTP = "paperboat-preview-http"
	AudienceTunnelHTTP  = "paperboat-tunnel-http"
	AudienceTunnelTCP   = "paperboat-tunnel-tcp"

	DecisionAllowed = "allowed"

	// The edge must renew a decision instead of retaining it indefinitely.
	DefaultDecisionTTL = 45 * time.Second
	MaximumDecisionTTL = 2 * time.Minute
	MaximumRequestBody = 64 << 10
	MaximumPathBytes   = 4096
	MaximumHostBytes   = 512
)

var (
	ErrInvalid             = errors.New("invalid private access request")
	ErrDenied              = errors.New("private access denied")
	ErrResourceNotFound    = errors.New("private access resource not found")
	ErrRouteUnavailable    = errors.New("private access route unavailable")
	ErrIdentityUnavailable = errors.New("private access identity unavailable")
	ErrAuditUnavailable    = errors.New("private access audit unavailable")
	ErrIdempotencyConflict = errors.New("private access idempotency conflict")
)

// DenyReason is stable machine-readable policy output.  The associated
// message is intentionally generic so a caller cannot use this API to probe
// another account's resources.
type DenyReason string

const (
	ReasonUnauthenticated  DenyReason = "unauthenticated"
	ReasonAccountMismatch  DenyReason = "account_mismatch"
	ReasonMachineRevoked   DenyReason = "machine_revoked"
	ReasonSessionExpired   DenyReason = "session_expired"
	ReasonResourceNotFound DenyReason = "resource_not_found"
	ReasonRoutePaused      DenyReason = "route_paused"
	ReasonExpired          DenyReason = "expired"
	ReasonWrongRoute       DenyReason = "wrong_route"
	ReasonProtocolDenied   DenyReason = "protocol_denied"
	ReasonIdentityInvalid  DenyReason = "identity_invalid"
	ReasonRateLimited      DenyReason = "rate_limited"
	ReasonInternal         DenyReason = "authorization_unavailable"
)

func (r DenyReason) valid() bool {
	switch r {
	case DenyReason(DecisionAllowed), ReasonUnauthenticated, ReasonAccountMismatch, ReasonMachineRevoked,
		ReasonSessionExpired, ReasonResourceNotFound, ReasonRoutePaused,
		ReasonExpired, ReasonWrongRoute, ReasonProtocolDenied,
		ReasonIdentityInvalid, ReasonRateLimited, ReasonInternal:
		return true
	default:
		return false
	}
}

func (r DenyReason) retryable() bool {
	return r == ReasonRoutePaused || r == ReasonRateLimited || r == ReasonInternal
}

// DeniedError is the typed policy failure returned by Authorize.  It contains
// no database text, credential, URL, or user-controlled value.
type DeniedError struct {
	Reason    DenyReason
	Retryable bool
	cause     error
}

func (e *DeniedError) Error() string {
	if e == nil {
		return ErrDenied.Error()
	}
	return "private access denied: " + string(e.Reason)
}

func (e *DeniedError) Unwrap() []error {
	if e == nil || e.cause == nil {
		return []error{ErrDenied}
	}
	return []error{ErrDenied, e.cause}
}

// DiagnosticCause exposes the wrapped internal cause to the owning HTTP
// boundary without including its text in Error or the public denial body.
func (e *DeniedError) DiagnosticCause() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newDenied(reason DenyReason) *DeniedError {
	return newDeniedWithCause(reason, nil)
}

func newDeniedWithCause(reason DenyReason, cause error) *DeniedError {
	if !reason.valid() {
		reason = ReasonInternal
	}
	return &DeniedError{Reason: reason, Retryable: reason.retryable(), cause: cause}
}

// Identity is produced by the renewable stable-host machine verifier.
// verifier.  AccountID is server-derived and must never be copied from the
// resource request.  Revoked and ExpiresAt are checked on every authorization
// call, including an idempotency replay.
type Identity struct {
	AccountID              string
	UserID                 string
	MachineID              string
	SessionID              string
	ExpiresAt              time.Time
	Revoked                bool
	Method                 string
	InstallationGeneration uint64
}

func (i Identity) Validate(now time.Time) error {
	for name, value := range map[string]string{
		"account_id": i.AccountID, "user_id": i.UserID,
		"machine_id": i.MachineID, "session_id": i.SessionID,
	} {
		if !validID(value) {
			return fmt.Errorf("%w: invalid %s", ErrInvalid, name)
		}
	}
	if i.InstallationGeneration == 0 {
		return fmt.Errorf("%w: invalid installation_generation", ErrInvalid)
	}
	if i.Revoked {
		return newDenied(ReasonMachineRevoked)
	}
	if i.ExpiresAt.IsZero() || (!now.IsZero() && !i.ExpiresAt.After(now)) {
		return newDenied(ReasonSessionExpired)
	}
	return nil
}

// EdgeIdentity is populated only after the existing edge mTLS/control
// verifier has authenticated the caller. It is kept outside Request because
// the edge must not be able to choose the node or process epoch it claims to
// represent.
type EdgeIdentity struct {
	NodeID       string
	ProcessEpoch string
}

func (e EdgeIdentity) Validate() error {
	if !validID(e.NodeID) || connectorprotocol.ValidateOpaqueEpoch(e.ProcessEpoch) != nil {
		return fmt.Errorf("%w: edge identity is invalid", ErrInvalid)
	}
	return nil
}

// Request contains the exact route and carrier fence supplied by the edge.
// Account identity comes exclusively from Identity and the server resolver. It is the signed
// carrier envelope the edge verifies, so both sides share one definition and one validation.
type Request = connectorprotocol.PrivateAccessRequest

// validateRequest is the shared wire validation with no expiry check (the service checks expiry
// against its own clock).
func validateRequest(r Request) error {
	if err := r.Validate(time.Time{}); err != nil {
		return fmt.Errorf("%w: invalid private access request", ErrInvalid)
	}
	return nil
}

func audienceFor(resourceKind, protocol string) string {
	switch {
	case resourceKind == ResourcePreview && protocol == ProtocolHTTP:
		return AudiencePreviewHTTP
	case resourceKind == ResourceTunnel && protocol == ProtocolHTTP:
		return AudienceTunnelHTTP
	case resourceKind == ResourceTunnel && protocol == ProtocolTCP:
		return AudienceTunnelTCP
	default:
		return ""
	}
}

// Lookup is the account-scoped query sent to the durable resolver.
type Lookup struct {
	AccountID string
	Request   Request
	Edge      EdgeIdentity
	Now       time.Time
}

// Binding is the authoritative resource/carrier state returned by a
// resolver.  It is not client input.  Preview bindings are derived from
// previewattachment.Attachment and tunnel bindings from tunnel resource and
// connector-session rows.
type Binding struct {
	AccountID        string
	ResourceKind     string
	ResourceID       string
	RouteID          string
	OperationID      string
	ConnectorID      string
	CarrierSessionID string

	OwnerMachineID string
	OwnerSessionID string

	RouteGeneration      uint64
	ProcessGeneration    uint64
	ConfigGeneration     uint64
	SessionGeneration    uint64
	AssignmentGeneration uint64
	Protocol             string
	AccessMode           string
	State                string
	ExpiresAt            time.Time
	Hostname             string
	PathPrefix           string
	EdgeNodeID           string
	EdgeProcessEpoch     string
}

func (b Binding) Validate(now time.Time) error {
	if b.ResourceKind != ResourcePreview && b.ResourceKind != ResourceTunnel {
		return fmt.Errorf("%w: binding resource kind is invalid", ErrInvalid)
	}
	for name, value := range map[string]string{
		"account_id": b.AccountID, "resource_id": b.ResourceID,
		"route_id": b.RouteID, "carrier_session_id": b.CarrierSessionID,
	} {
		if !validID(value) {
			return fmt.Errorf("%w: binding %s is invalid", ErrInvalid, name)
		}
	}
	if b.ResourceKind == ResourcePreview && (!validID(b.OperationID) || !validID(b.OwnerMachineID) || !validID(b.OwnerSessionID)) {
		return fmt.Errorf("%w: preview binding owner or operation is invalid", ErrInvalid)
	}
	if b.ResourceKind == ResourceTunnel && !validID(b.ConnectorID) {
		return fmt.Errorf("%w: tunnel binding connector is invalid", ErrInvalid)
	}
	if b.Protocol != ProtocolHTTP && b.Protocol != ProtocolTCP {
		return fmt.Errorf("%w: binding protocol is invalid", ErrInvalid)
	}
	if b.AccessMode != "private" {
		return newDenied(ReasonProtocolDenied)
	}
	if b.RouteGeneration == 0 || b.SessionGeneration == 0 || b.ProcessGeneration == 0 || b.ConfigGeneration == 0 || b.AssignmentGeneration == 0 {
		return fmt.Errorf("%w: binding generations must be positive", ErrInvalid)
	}
	if b.ExpiresAt.IsZero() || (!now.IsZero() && !b.ExpiresAt.After(now)) {
		return newDenied(ReasonExpired)
	}
	if b.Protocol == ProtocolHTTP && (len(b.Hostname) == 0 || len(b.Hostname) > MaximumHostBytes || hasControl(b.Hostname)) {
		return fmt.Errorf("%w: binding host is invalid", ErrInvalid)
	}
	if (b.EdgeNodeID == "") != (b.EdgeProcessEpoch == "") {
		return fmt.Errorf("%w: binding edge identity is incomplete", ErrInvalid)
	}
	if b.EdgeNodeID != "" {
		if err := (EdgeIdentity{NodeID: b.EdgeNodeID, ProcessEpoch: b.EdgeProcessEpoch}).Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Decision is safe to send to an edge: it carries no access token, bearer, credential, private key
// or target secret. The edge must ask again after ExpiresAt and must close active streams when its
// policy cache expires. Reason is a DenyReason on the wire.
type Decision = connectorprotocol.PrivateAccessDecision

func validateDecision(d Decision, now time.Time) error {
	if d.Schema != Schema || d.Kind != Kind || !validID(d.DecisionID) || !DenyReason(d.Reason).valid() || d.ExpiresAt.IsZero() || (!now.IsZero() && !d.ExpiresAt.After(now)) {
		return fmt.Errorf("%w: malformed decision", ErrInvalid)
	}
	if d.Allowed != (d.Reason == string(DecisionAllowed)) {
		return fmt.Errorf("%w: decision allow/reason mismatch", ErrInvalid)
	}
	if len(d.RequestID) < 3 || len(d.RequestID) > 128 || len(d.CorrelationID) < 3 || len(d.CorrelationID) > 128 {
		return fmt.Errorf("%w: malformed decision trace fields", ErrInvalid)
	}
	return nil
}

// ResourceResolver is the only place allowed to turn public IDs into an
// authoritative private route binding.
type ResourceResolver interface {
	ResolvePrivate(context.Context, Lookup) (Binding, error)
}

// MachineStateVerifier performs the fresh revocation and installation check
// used after a grant has been verified at the edge.
type MachineStateVerifier interface {
	VerifyCurrentMachine(context.Context, Identity, time.Time) error
	VerifyAdmittedMachine(context.Context, Identity, time.Time) error
}

// GrantVerifier validates a server-signed, short-lived route grant.  The
// verifier must return the signed request claims, not arbitrary caller input.
type GrantVerifier interface {
	VerifyGrant(context.Context, string, time.Time) (Request, error)
	VerifyAdmittedGrant(context.Context, string, time.Time) (Request, error)
}

// AuditRecord is deliberately a safe projection.  Implementations must not
// add request headers, cookies, URLs with credentials, or proof bytes.
type AuditRecord struct {
	EventType        string
	Allowed          bool
	Reason           DenyReason
	AccountID        string
	UserID           string
	MachineID        string
	SessionID        string
	ResourceKind     string
	ResourceID       string
	RouteID          string
	OperationID      string
	ConnectorID      string
	CarrierSessionID string
	Protocol         string
	RequestID        string
	CorrelationID    string
	IdempotencyKey   string
}

type AuditSink interface {
	Record(context.Context, AuditRecord) error
}

func requestFingerprint(in AuthorizeInput) (string, error) {
	// Identity is intentionally reduced to server-owned stable IDs. Proof bytes
	// never enter the replay key, audit record, or decision body.
	envelope := struct {
		AccountID string  `json:"account_id"`
		UserID    string  `json:"user_id"`
		MachineID string  `json:"machine_id"`
		SessionID string  `json:"session_id"`
		Request   Request `json:"request"`
	}{in.Identity.AccountID, in.Identity.UserID, in.Identity.MachineID, in.Identity.SessionID, in.Request}
	b, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// requestHash returns the canonical digest bound into a signed private-access grant.
// It includes route, identity, expiry, nonce, and trace fields but never any
// bearer or proof bytes.
func requestHash(r Request) (string, error) {
	if err := validateRequest(r); err != nil {
		return "", err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("%w: hash request", ErrInvalid)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func decisionID(fingerprint string) string {
	return "pad_" + fingerprint[:32]
}

func validID(value string) bool {
	if len(value) == 0 || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e || strings.ContainsRune("/\\?&=#", r) {
			return false
		}
	}
	return true
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func canonicalHost(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if host, _, err := net.SplitHostPort(value); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.TrimSuffix(value, ".")
}

func hostFromEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}
