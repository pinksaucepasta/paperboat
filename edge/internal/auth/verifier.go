package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/admission"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgeerrors"
	"github.com/pinksaucepasta/paperboat/shared/strictjson"
)

const maxCredentialBytes = 8192

type KeySource interface {
	Key(context.Context, string) (ed25519.PublicKey, error)
}
type Revocations interface {
	Revoked(context.Context, admission.Claims) (bool, error)
}

type Verifier struct {
	Issuer      string
	NodeID      string
	Keys        KeySource
	Revocations Revocations
	Now         func() time.Time
	ClockSkew   time.Duration
}

type header struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}
type claims struct {
	WorkspaceID            string   `json:"workspace_id,omitempty"`
	ActorAccountID         string   `json:"actor_account_id,omitempty"`
	AssignmentID           string   `json:"assignment_id,omitempty"`
	BrowserAttachmentID    string   `json:"browser_attachment_id,omitempty"`
	BrowserPublicKeySHA256 string   `json:"browser_public_key_sha256,omitempty"`
	PolicyGeneration       int64    `json:"policy_generation,omitempty"`
	Issuer                 string   `json:"iss"`
	Audience               string   `json:"aud"`
	Subject                string   `json:"sub"`
	JTI                    string   `json:"jti"`
	IssuedAt               int64    `json:"iat"`
	Expires                int64    `json:"exp"`
	Scope                  []string `json:"scope"`
	CredentialClass        string   `json:"credential_class"`
	EnvironmentID          string   `json:"environment_id"`
	MachineID              string   `json:"machine_id"`
	InstallationGeneration int64    `json:"installation_generation"`
	SourceMachineID        string   `json:"source_machine_id,omitempty"`
	HelperID               string   `json:"helper_id"`
	ConnectorID            string   `json:"connector_id"`
	ConnectorGeneration    uint64   `json:"connector_generation"`
	EdgePool               string   `json:"edge_pool"`
	EdgeNodeID             string   `json:"edge_node_id"`
	AccountID              string   `json:"account_id,omitempty"`
	UserID                 string   `json:"user_id"`
	ActorID                string   `json:"actor_id,omitempty"`
	CLIClientSessionID     string   `json:"cli_client_session_id"`
	SessionID              string   `json:"session_id"`
	OperationID            string   `json:"operation_id,omitempty"`
	ResourceKind           string   `json:"resource_kind,omitempty"`
	ResourceID             string   `json:"resource_id,omitempty"`
	PreviewID              string   `json:"preview_id,omitempty"`
	OwnerSessionID         string   `json:"owner_session_id,omitempty"`
	OwnerSessionKind       string   `json:"owner_session_kind,omitempty"`
	IdempotencyKey         string   `json:"idempotency_key,omitempty"`
	RequestID              string   `json:"request_id,omitempty"`
	CorrelationID          string   `json:"correlation_id,omitempty"`
	TargetScheme           string   `json:"target_scheme,omitempty"`
	TargetAddress          string   `json:"target_address,omitempty"`
	AccessMode             string   `json:"access_mode,omitempty"`
	PreviewEndpoint        string   `json:"endpoint,omitempty"`
	LeaseDeadline          int64    `json:"lease_deadline,omitempty"`
	UserDeadline           *int64   `json:"user_deadline,omitempty"`
	LeaseETag              string   `json:"lease_etag,omitempty"`
	State                  string   `json:"state,omitempty"`
	AllocationState        string   `json:"allocation_state,omitempty"`
	EdgeState              string   `json:"edge_state,omitempty"`
	OriginState            string   `json:"origin_state,omitempty"`
	CreatedAt              int64    `json:"created_at,omitempty"`
	LastRenewedAt          int64    `json:"last_renewed_at,omitempty"`
	ExpectedGeneration     int64    `json:"expected_generation,omitempty"`
	RequestHash            string   `json:"request_hash,omitempty"`
	IntentID               string   `json:"intent_id"`
	EndpointID             string   `json:"endpoint_id"`
	PeerEndpointID         string   `json:"peer_endpoint_id"`
	AttemptGeneration      uint64   `json:"attempt_generation"`
	NetworkGeneration      uint64   `json:"network_generation"`
	PeerRole               string   `json:"peer_role"`
	RouteAllocation        string   `json:"route_allocation"`
	RouteGeneration        uint64   `json:"route_generation"`
	InitiatorEndpointID    string   `json:"initiator_endpoint_id"`
	ResponderEndpointID    string   `json:"responder_endpoint_id"`
	RelayByteLimit         uint64   `json:"relay_byte_limit"`
	RelayCarriers          []string `json:"relay_carriers"`
}

func (v *Verifier) VerifyHelperAccess(ctx context.Context, token string) (admission.Claims, error) {
	if v == nil || v.Keys == nil || v.Issuer == "" || len(token) == 0 || len(token) > maxCredentialBytes || v.ClockSkew < 0 || v.ClockSkew > time.Minute {
		return admission.Claims{}, invalid()
	}
	parsedHeader, parsed, err := v.verifySigned(ctx, token)
	if err != nil {
		return admission.Claims{}, err
	}
	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now().UTC()
	}
	var wantScopes []string
	switch parsed.CredentialClass {
	case "browser_terminal_operation":
		if exactScopes(parsed.Scope, []string{"terminal:view"}) {
			wantScopes = []string{"terminal:view"}
		} else if exactScopes(parsed.Scope, []string{"terminal:control"}) {
			wantScopes = []string{"terminal:control"}
		} else {
			wantScopes = []string{"terminal:operate"}
		}
	case "terminal_operation":
		wantScopes = []string{"terminal:operate"}
	case "file_transfer":
		wantScopes = []string{"file:transfer"}
	case "inbox_list":
		wantScopes = []string{"inbox:list"}
	case "inbox_read":
		wantScopes = []string{"inbox:read"}
	case "inbox_delete":
		wantScopes = []string{"inbox:delete"}
	case "preview_launch":
		if parsed.OwnerSessionKind != "local_lease" && parsed.OwnerSessionKind != "foreground" && parsed.OwnerSessionKind != "lazy_runtime" {
			return admission.Claims{}, invalid()
		}
		wantScopes = []string{"preview:launch"}
	default:
		return admission.Claims{}, invalid()
	}
	if parsed.CredentialClass == "terminal_operation" || parsed.CredentialClass == "browser_terminal_operation" {
		if parsed.WorkspaceID == "" || len(parsed.WorkspaceID) > 128 || strings.TrimSpace(parsed.WorkspaceID) != parsed.WorkspaceID || strings.ContainsAny(parsed.WorkspaceID, "\x00\r\n") {
			return admission.Claims{}, invalid()
		}
		guestRead := parsed.CredentialClass == "browser_terminal_operation" && parsed.ActorAccountID == "" && len(parsed.Scope) == 1 && (parsed.Scope[0] == "terminal:view" || parsed.Scope[0] == "terminal:control")
		if !guestRead && (parsed.ActorAccountID == "" || parsed.ActorAccountID != parsed.UserID) {
			return admission.Claims{}, invalid()
		}
	}
	// preview_launch is a control-plane-to-host credential. It may originate
	// from a browser or a CLI request, so it binds the authenticated user and
	// operation but deliberately has no CLI client-session claim. Interactive
	// terminal and transfer credentials remain client-session bound.
	// The inbox credentials are control-plane-to-host credentials too: a console request, no CLI session.
	requiresClientSession := parsed.CredentialClass != "preview_launch" && parsed.CredentialClass != "browser_terminal_operation" && !strings.HasPrefix(parsed.CredentialClass, "inbox_")
	if parsed.CredentialClass == "browser_terminal_operation" {
		digest, e := base64.RawURLEncoding.Strict().DecodeString(parsed.BrowserPublicKeySHA256)
		if e != nil || len(digest) != 32 || parsed.BrowserAttachmentID == "" || parsed.SessionID == "" || parsed.ExpectedGeneration < 1 || parsed.PolicyGeneration < 1 || parsed.CLIClientSessionID != "" || parsed.SourceMachineID != "" || parsed.AccountID != parsed.UserID || parsed.Subject != parsed.UserID {
			return admission.Claims{}, invalid()
		}
	}
	if parsed.Issuer != v.Issuer || parsed.Audience != "paperboat-machine" || parsed.Subject == "" || parsed.JTI == "" || !exactScopes(parsed.Scope, wantScopes) || parsed.EnvironmentID == "" || parsed.MachineID == "" || parsed.CredentialClass == "file_transfer" && parsed.SourceMachineID == "" || parsed.CredentialClass == "terminal_operation" && parsed.SessionID == "" || parsed.UserID == "" || requiresClientSession && parsed.CLIClientSessionID == "" || parsed.Expires <= parsed.IssuedAt || parsed.Expires-parsed.IssuedAt > 300 || time.Unix(parsed.IssuedAt, 0).After(now.Add(v.ClockSkew)) || !time.Unix(parsed.Expires, 0).After(now) {
		return admission.Claims{}, invalid()
	}
	result := admission.Claims{KeyID: parsedHeader.KeyID, Issuer: parsed.Issuer, Audience: parsed.Audience, JTI: parsed.JTI, CredentialClass: parsed.CredentialClass, Scopes: append([]string(nil), parsed.Scope...), EnvironmentID: parsed.EnvironmentID, MachineID: parsed.MachineID, InstallationGeneration: parsed.InstallationGeneration, HelperID: parsed.HelperID, ConnectorID: parsed.ConnectorID, EdgePool: parsed.EdgePool, EdgeNodeID: parsed.EdgeNodeID, ConnectorGeneration: parsed.ConnectorGeneration, AssignmentID: parsed.AssignmentID, IssuedAt: time.Unix(parsed.IssuedAt, 0).UTC(), ExpiresAt: time.Unix(parsed.Expires, 0).UTC()}
	if v.Revocations != nil {
		revoked, err := v.Revocations.Revoked(ctx, result)
		if err != nil {
			return admission.Claims{}, edgeerrors.Wrap(edgeerrors.CodeCredentialRevocationUnavailable, "credential revocation state is unavailable", "retry after revocation synchronization", err)
		}
		result.Revoked = revoked
	}
	return result, nil
}

func exactScopes(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	remaining := make(map[string]bool, len(expected))
	for _, scope := range expected {
		if scope == "" || remaining[scope] {
			return false
		}
		remaining[scope] = true
	}
	for _, scope := range actual {
		if !remaining[scope] {
			return false
		}
		delete(remaining, scope)
	}
	return len(remaining) == 0
}

func (v *Verifier) verifySigned(ctx context.Context, token string) (header, claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return header{}, claims{}, invalid()
	}
	headerBytes, err := decodeCredentialSegment(parts[0])
	if err != nil {
		return header{}, claims{}, invalid()
	}
	var parsedHeader header
	if strictJSON(headerBytes, &parsedHeader) != nil || parsedHeader.Algorithm != "EdDSA" || parsedHeader.Type != "paperboat-credential+jwt" || parsedHeader.KeyID == "" || len(parsedHeader.KeyID) > 128 {
		return header{}, claims{}, invalid()
	}
	key, err := v.Keys.Key(ctx, parsedHeader.KeyID)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return header{}, claims{}, edgeerrors.Wrap(edgeerrors.CodeCredentialKeyUnavailable, "credential signing key is unavailable", "retry after key synchronization", err)
	}
	signature, err := decodeCredentialSegment(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return header{}, claims{}, invalid()
	}
	payload, err := decodeCredentialSegment(parts[1])
	if err != nil {
		return header{}, claims{}, invalid()
	}
	var parsed claims
	if strictJSON(payload, &parsed) != nil {
		return header{}, claims{}, invalid()
	}
	return parsedHeader, parsed, nil
}

func strictJSON(data []byte, target any) error {
	return strictjson.Decode(data, target, maxSnapshotJSONDepth)
}

func decodeCredentialSegment(encoded string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, errors.New("credential segment is not canonical base64url")
	}
	return decoded, nil
}

func invalid() error {
	return edgeerrors.New(edgeerrors.CodeCredentialInvalid, "credential is invalid", "request a fresh admission")
}

type StaticKeys map[string]ed25519.PublicKey

func (keys StaticKeys) Key(_ context.Context, keyID string) (ed25519.PublicKey, error) {
	key, ok := keys[keyID]
	if !ok {
		return nil, errors.New("unknown key")
	}
	return append(ed25519.PublicKey(nil), key...), nil
}
