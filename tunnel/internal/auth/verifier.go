package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/admission"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/edgeerrors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/strictjson"
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
type fileTransferPolicy struct {
	Revision               string `json:"revision"`
	MaxFileBytes           int64  `json:"max_file_bytes"`
	MaxBatchFiles          int    `json:"max_batch_files"`
	MaxBatchBytes          int64  `json:"max_batch_bytes"`
	MaxConcurrentTransfers int    `json:"max_concurrent_transfers"`
	RetentionSeconds       int64  `json:"retention_seconds"`
	DeliveryTimeoutSeconds int64  `json:"delivery_timeout_seconds"`
	MaxPendingSpoolBytes   int64  `json:"max_pending_spool_bytes"`
}
type claims struct {
	Issuer                 string              `json:"iss"`
	Audience               string              `json:"aud"`
	Subject                string              `json:"sub"`
	JTI                    string              `json:"jti"`
	IssuedAt               int64               `json:"iat"`
	Expires                int64               `json:"exp"`
	Scope                  []string            `json:"scope"`
	CredentialClass        string              `json:"credential_class"`
	EnvironmentID          string              `json:"environment_id"`
	MachineID              string              `json:"machine_id"`
	InstallationGeneration int64               `json:"installation_generation"`
	SourceMachineID        string              `json:"source_machine_id,omitempty"`
	HelperID               string              `json:"helper_id"`
	ConnectorID            string              `json:"connector_id"`
	ConnectorGeneration    uint64              `json:"connector_generation"`
	EdgePool               string              `json:"edge_pool"`
	EdgeNodeID             string              `json:"edge_node_id"`
	RouteBinding           string              `json:"route_binding"`
	FileTransferPolicy     *fileTransferPolicy `json:"file_transfer_policy"`
	AccountID              string              `json:"account_id,omitempty"`
	UserID                 string              `json:"user_id"`
	ActorID                string              `json:"actor_id,omitempty"`
	CLIClientSessionID     string              `json:"cli_client_session_id"`
	SessionID              string              `json:"session_id"`
	OperationID            string              `json:"operation_id,omitempty"`
	PreviewID              string              `json:"preview_id,omitempty"`
	OwnerSessionID         string              `json:"owner_session_id,omitempty"`
	IdempotencyKey         string              `json:"idempotency_key,omitempty"`
	RequestID              string              `json:"request_id,omitempty"`
	CorrelationID          string              `json:"correlation_id,omitempty"`
	TargetScheme           string              `json:"target_scheme,omitempty"`
	TargetAddress          string              `json:"target_address,omitempty"`
	AccessMode             string              `json:"access_mode,omitempty"`
	PreviewEndpoint        string              `json:"endpoint,omitempty"`
	LeaseDeadline          int64               `json:"lease_deadline,omitempty"`
	UserDeadline           *int64              `json:"user_deadline,omitempty"`
	LeaseETag              string              `json:"lease_etag,omitempty"`
	State                  string              `json:"state,omitempty"`
	AllocationState        string              `json:"allocation_state,omitempty"`
	EdgeState              string              `json:"edge_state,omitempty"`
	OriginState            string              `json:"origin_state,omitempty"`
	CreatedAt              int64               `json:"created_at,omitempty"`
	LastRenewedAt          int64               `json:"last_renewed_at,omitempty"`
	ExpectedGeneration     int64               `json:"expected_generation,omitempty"`
	RequestHash            string              `json:"request_hash,omitempty"`
	IntentID               string              `json:"intent_id"`
	EndpointID             string              `json:"endpoint_id"`
	PeerEndpointID         string              `json:"peer_endpoint_id"`
	AttemptGeneration      uint64              `json:"attempt_generation"`
	NetworkGeneration      uint64              `json:"network_generation"`
	PeerRole               string              `json:"peer_role"`
	RouteAllocation        string              `json:"route_allocation"`
	RouteGeneration        uint64              `json:"route_generation"`
	InitiatorEndpointID    string              `json:"initiator_endpoint_id"`
	ResponderEndpointID    string              `json:"responder_endpoint_id"`
	RelayByteLimit         uint64              `json:"relay_byte_limit"`
	RelayCarriers          []string            `json:"relay_carriers"`
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
	case "terminal_operation":
		wantScopes = []string{"terminal:operate"}
	case "file_transfer":
		wantScopes = []string{"file:transfer"}
	case "codex_connect":
		wantScopes = []string{"codex:connect"}
	case "codex_manage":
		wantScopes = []string{"codex:prepare", "codex:browse", "codex:renew", "codex:stop"}
	case "preview_launch":
		wantScopes = []string{"preview:launch"}
	default:
		return admission.Claims{}, invalid()
	}
	codexCredential := parsed.CredentialClass == "codex_connect" || parsed.CredentialClass == "codex_manage"
	// preview_launch is a control-plane-to-host credential. It may originate
	// from a browser or a CLI request, so it binds the authenticated user and
	// operation but deliberately has no CLI client-session claim. Interactive
	// terminal, transfer, and Codex credentials remain client-session bound.
	requiresClientSession := parsed.CredentialClass != "preview_launch"
	if parsed.Issuer != v.Issuer || parsed.Audience != "paperboat-machine" || parsed.Subject == "" || parsed.JTI == "" || !exactScopes(parsed.Scope, wantScopes) || parsed.EnvironmentID == "" || parsed.MachineID == "" || parsed.CredentialClass == "file_transfer" && parsed.SourceMachineID == "" || parsed.CredentialClass == "terminal_operation" && parsed.SessionID == "" || codexCredential && (parsed.SessionID == "" || parsed.InstallationGeneration < 1 || parsed.ConnectorID == "" || parsed.ConnectorGeneration < 1 || parsed.EdgePool == "" || parsed.EdgeNodeID == "") || parsed.UserID == "" || requiresClientSession && parsed.CLIClientSessionID == "" || parsed.Expires <= parsed.IssuedAt || parsed.Expires-parsed.IssuedAt > 300 || time.Unix(parsed.IssuedAt, 0).After(now.Add(v.ClockSkew)) || !time.Unix(parsed.Expires, 0).After(now) {
		return admission.Claims{}, invalid()
	}
	result := admission.Claims{KeyID: parsedHeader.KeyID, Issuer: parsed.Issuer, Audience: parsed.Audience, JTI: parsed.JTI, CredentialClass: parsed.CredentialClass, Scopes: append([]string(nil), parsed.Scope...), EnvironmentID: parsed.EnvironmentID, MachineID: parsed.MachineID, HelperID: parsed.HelperID, ConnectorGeneration: parsed.ConnectorGeneration, ExpiresAt: time.Unix(parsed.Expires, 0).UTC()}
	if v.Revocations != nil {
		revoked, err := v.Revocations.Revoked(ctx, result)
		if err != nil {
			return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialInvalid, "credential revocation state is unavailable", "retry after revocation synchronization")
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
		return header{}, claims{}, edgeerrors.New(edgeerrors.CodeCredentialInvalid, "credential signing key is unavailable", "retry after key synchronization")
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

func (v *Verifier) Verify(ctx context.Context, token string) (admission.Claims, error) {
	if v == nil || v.Keys == nil || v.Issuer == "" || v.ClockSkew < 0 || v.ClockSkew > time.Minute {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeServiceUnavailable, "credential verifier is unavailable", "retry after edge recovery")
	}
	if len(token) == 0 || len(token) > maxCredentialBytes {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialMalformed, "credential size is invalid", "request a fresh admission")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialMalformed, "credential structure is invalid", "request a fresh admission")
	}
	headerBytes, err := decodeCredentialSegment(parts[0])
	if err != nil {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialMalformed, "credential header is malformed", "request a fresh admission")
	}
	var parsedHeader header
	if strictJSON(headerBytes, &parsedHeader) != nil || parsedHeader.Algorithm != "EdDSA" || parsedHeader.Type != "paperboat-credential+jwt" || parsedHeader.KeyID == "" || len(parsedHeader.KeyID) > 128 {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialMalformed, "credential header is invalid", "request a fresh admission")
	}
	key, err := v.Keys.Key(ctx, parsedHeader.KeyID)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialKeyUnavailable, "credential signing key is unavailable", "retry after key synchronization")
	}
	signature, err := decodeCredentialSegment(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialSignatureInvalid, "credential signature is invalid", "request a fresh admission")
	}
	payload, err := decodeCredentialSegment(parts[1])
	if err != nil {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialMalformed, "credential payload is malformed", "request a fresh admission")
	}
	var parsed claims
	if strictJSON(payload, &parsed) != nil {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialMalformed, "credential payload is malformed", "request a fresh admission")
	}
	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now().UTC()
	}
	routeBinding, routeBindingErr := base64.RawURLEncoding.Strict().DecodeString(parsed.RouteBinding)
	if parsed.Issuer != v.Issuer || parsed.Audience != "paperboat-edge" || parsed.Subject == "" || parsed.JTI == "" || parsed.CredentialClass != "connector_admission" || len(parsed.Scope) != 1 || parsed.Scope[0] != "connector:admit" || parsed.EnvironmentID == "" || parsed.MachineID == "" || parsed.InstallationGeneration < 1 || parsed.ConnectorID == "" || parsed.ConnectorGeneration == 0 || parsed.EdgePool == "" || parsed.EdgeNodeID == "" || routeBindingErr != nil || len(routeBinding) != 32 || base64.RawURLEncoding.EncodeToString(routeBinding) != parsed.RouteBinding || !validFileTransferPolicy(parsed.FileTransferPolicy) || parsed.Expires <= parsed.IssuedAt || parsed.Expires-parsed.IssuedAt > 300 {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeBindingInvalid, "credential claims are invalid", "request a fresh admission")
	}
	if time.Unix(parsed.IssuedAt, 0).After(now.Add(v.ClockSkew)) {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialNotYetValid, "credential is not yet valid", "synchronize clocks and request a fresh admission")
	}
	if !time.Unix(parsed.Expires, 0).After(now) {
		return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialExpired, "credential is expired", "request a fresh admission")
	}
	result := admission.Claims{KeyID: parsedHeader.KeyID, Issuer: parsed.Issuer, Audience: parsed.Audience, JTI: parsed.JTI, CredentialClass: parsed.CredentialClass, Scopes: append([]string(nil), parsed.Scope...), EnvironmentID: parsed.EnvironmentID, MachineID: parsed.MachineID, InstallationGeneration: parsed.InstallationGeneration, ConnectorID: parsed.ConnectorID, ConnectorGeneration: parsed.ConnectorGeneration, EdgePool: parsed.EdgePool, EdgeNodeID: parsed.EdgeNodeID, RouteBinding: parsed.RouteBinding, ExpiresAt: time.Unix(parsed.Expires, 0).UTC()}
	if v.Revocations != nil {
		revoked, err := v.Revocations.Revoked(ctx, result)
		if err != nil {
			return admission.Claims{}, edgeerrors.New(edgeerrors.CodeCredentialRevocationUnavailable, "credential revocation state is unavailable", "retry after revocation synchronization")
		}
		if revoked {
			result.Revoked = true
		}
	}
	return result, nil
}

func validFileTransferPolicy(policy *fileTransferPolicy) bool {
	return policy != nil && policy.Revision != "" && policy.MaxFileBytes > 0 && policy.MaxFileBytes <= 50<<20 && policy.MaxBatchFiles > 0 && policy.MaxBatchFiles <= 10 && policy.MaxBatchBytes >= policy.MaxFileBytes && policy.MaxBatchBytes <= 500<<20 && policy.MaxConcurrentTransfers > 0 && policy.MaxConcurrentTransfers <= 2 && policy.RetentionSeconds > 0 && policy.DeliveryTimeoutSeconds > 0 && policy.MaxPendingSpoolBytes >= policy.MaxBatchBytes
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
