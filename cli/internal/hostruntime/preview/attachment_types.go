package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

const (
	PreviewCarrierAttachmentKind = "preview_carrier_attachment"
	attachmentRequestMaxBytes    = 64 << 10
	attachmentResponseMaxBytes   = 64 << 10
	attachmentProofMaxBytes      = 16 << 10
	attachmentIdentityMaxBytes   = 16 << 10
	attachmentAdmissionPoll      = 250 * time.Millisecond
)

var (
	ErrAttachmentClientInvalid     = errors.New("invalid preview carrier attachment client")
	ErrAttachmentClientUnavailable = errors.New("preview carrier attachment service unavailable")
	ErrAttachmentClientRejected    = errors.New("preview carrier attachment request rejected")
	ErrAttachmentBinding           = errors.New("preview carrier attachment binding is invalid")
	ErrAttachmentSessionInvalid    = errors.New("preview carrier attachment session is invalid")
	ErrAttachmentLeaseETagStale    = errors.New("preview lease ETag is stale")
	ErrAttachmentAdmissionPending  = errors.New("preview carrier attachment is waiting for edge admission")
)

// AttachmentAdmissionWaiter is implemented by an attachment transport that
// can replay the same durable allocation until the server has accepted the
// edge admission. The request and proof envelope remain unchanged; only the
// server-issued attachment state/generation may advance. It returns as soon
// as the server reports admitted, edge_ready, or ready. A carrier must use
// AttachmentEdgeReadyWaiter after it has connected when it needs to wait for
// the edge to observe that carrier.
type AttachmentAdmissionWaiter interface {
	WaitForAdmission(context.Context, AttachmentRequest, Attachment) (Attachment, error)
}

// AttachmentEdgeReadyWaiter waits for the edge-side observation of an
// already-admitted carrier. This is a separate phase because the host must
// connect the carrier while the attachment is only admitted; waiting for
// edge_ready before dialing creates a circular handshake.
type AttachmentEdgeReadyWaiter interface {
	WaitForEdgeReady(context.Context, AttachmentRequest, Attachment) (Attachment, error)
}

// AttachmentReadinessObserver records the owner's origin probe after the
// edge has accepted the server-issued admission. It is deliberately
// separate from Allocation so a pending/admitted response can never be
// mistaken for lease readiness.
type AttachmentReadinessObserver interface {
	ObserveOrigin(context.Context, AttachmentRequest, Attachment, bool) (Attachment, error)
}

// AttachmentRequest is the secret-free operation envelope accepted by the
// server's POST /v1/previews/{preview_id}/carrier-attachment endpoint.
// OperationID and IdempotencyKey are intentionally the same value. The
// machine proof is bound to that exact operation so a retry cannot be
// repurposed for another attachment.
type AttachmentRequest struct {
	PreviewID      string `json:"preview_id"`
	OperationID    string `json:"operation_id"`
	OwnerMachineID string `json:"owner_machine_id"`
	OwnerSessionID string `json:"owner_session_id"`
	IdempotencyKey string `json:"idempotency_key"`
	RequestID      string `json:"request_id"`
	CorrelationID  string `json:"correlation_id"`
	// LeaseETag is transport metadata for the lease precondition. It is
	// intentionally excluded from the signed request body and must be sent
	// verbatim as If-Match by AttachmentClient.
	LeaseETag string `json:"-"`
}

// AttachmentRequestForLease builds the attachment request from the immutable
// server create operation retained on Lease. It intentionally refuses to
// substitute a client idempotency key or preview ID when that operation is
// unavailable.
func AttachmentRequestForLease(lease Lease, requestID, correlationID string) (AttachmentRequest, error) {
	if !validLeaseID(lease.ID) || !validLeaseID(lease.OwnerMachineID) || !validLeaseID(lease.OwnerSessionID) || !validLeaseID(lease.CreateOperationID) {
		return AttachmentRequest{}, fmt.Errorf("%w: lease has no durable create operation", ErrAttachmentBinding)
	}
	request := AttachmentRequest{
		PreviewID: lease.ID, OperationID: lease.CreateOperationID,
		OwnerMachineID: lease.OwnerMachineID, OwnerSessionID: lease.OwnerSessionID,
		IdempotencyKey: lease.CreateOperationID, RequestID: requestID, CorrelationID: correlationID,
		LeaseETag: strings.TrimSpace(lease.ETag),
	}
	if err := request.Validate(); err != nil {
		return AttachmentRequest{}, err
	}
	if strings.TrimSpace(lease.ETag) == "" {
		return AttachmentRequest{}, errors.Join(ErrAttachmentBinding, api.ErrPreviewLeaseETagRequired)
	}
	if err := api.ValidatePreviewLeaseETag(lease.ID, lease.ETag); err != nil {
		return AttachmentRequest{}, fmt.Errorf("%w: lease ETag: %v", ErrAttachmentBinding, err)
	}
	return request, nil
}

func (r AttachmentRequest) Validate() error {
	if !validAttachmentID(r.PreviewID) || !validAttachmentID(r.OperationID) || !validAttachmentID(r.OwnerMachineID) || !validAttachmentID(r.OwnerSessionID) {
		return fmt.Errorf("%w: incomplete attachment request", ErrAttachmentClientInvalid)
	}
	if r.OperationID != r.IdempotencyKey || !validAttachmentID(r.IdempotencyKey) {
		return fmt.Errorf("%w: operation and idempotency keys must match", ErrAttachmentClientInvalid)
	}
	if len(r.RequestID) < 3 || len(r.RequestID) > 128 || len(r.CorrelationID) < 3 || len(r.CorrelationID) > 128 || hasAttachmentControl(r.RequestID) || hasAttachmentControl(r.CorrelationID) {
		return fmt.Errorf("%w: invalid request or correlation ID", ErrAttachmentClientInvalid)
	}
	if r.LeaseETag != "" {
		if err := api.ValidatePreviewLeaseETag(r.PreviewID, r.LeaseETag); err != nil {
			return fmt.Errorf("%w: lease ETag: %v", ErrAttachmentBinding, err)
		}
	}
	return nil
}

// Hash is the server-compatible canonical request hash. It binds the
// authoritative account returned in the attachment to every request field,
// without ever including a credential or proof.
func (r AttachmentRequest) Hash(accountID string) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	if !validAttachmentID(accountID) {
		return "", fmt.Errorf("%w: invalid account ID", ErrAttachmentClientInvalid)
	}
	envelope := struct {
		AccountID      string `json:"account_id"`
		PreviewID      string `json:"preview_id"`
		OperationID    string `json:"operation_id"`
		OwnerMachineID string `json:"owner_machine_id"`
		OwnerSessionID string `json:"owner_session_id"`
		IdempotencyKey string `json:"idempotency_key"`
		RequestID      string `json:"request_id"`
		CorrelationID  string `json:"correlation_id"`
	}{accountID, r.PreviewID, r.OperationID, r.OwnerMachineID, r.OwnerSessionID, r.IdempotencyKey, r.RequestID, r.CorrelationID}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("%w: canonical request: %v", ErrAttachmentClientInvalid, err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Binding is the server-authenticated identity and generation projection.
// It contains no bearer, private-key, or renewable credential material.
type Binding struct {
	AccountID                            string `json:"account_id"`
	PreviewID                            string `json:"preview_id"`
	OperationID                          string `json:"operation_id"`
	OwnerMachineID                       string `json:"owner_machine_id"`
	OwnerSessionID                       string `json:"owner_session_id"`
	HostID                               string `json:"host_id"`
	LeaseGeneration                      uint64 `json:"lease_generation"`
	TunnelID                             string `json:"tunnel_id"`
	ConnectorID                          string `json:"connector_id"`
	SessionID                            string `json:"session_id"`
	ProcessGeneration                    uint64 `json:"process_generation"`
	ConfigGeneration                     uint64 `json:"config_generation"`
	RouteID                              string `json:"route_id"`
	RouteGeneration                      uint64 `json:"route_generation"`
	EdgeNodeID                           string `json:"edge_node_id"`
	EdgeProcessEpoch                     string `json:"edge_process_epoch"`
	EdgeCarrierServerSPKISHA256          string `json:"edge_carrier_server_spki_sha256"`
	EdgeCarrierServerCertificateChainPEM string `json:"edge_carrier_server_certificate_chain_pem"`
	MachineIdentityPublicKey             string `json:"machine_identity_public_key"`
	MachineIdentityThumbprint            string `json:"machine_identity_thumbprint"`
}

func (b Binding) Validate() error {
	for name, value := range map[string]string{
		"account_id": b.AccountID, "preview_id": b.PreviewID, "operation_id": b.OperationID,
		"owner_machine_id": b.OwnerMachineID, "owner_session_id": b.OwnerSessionID,
		"host_id": b.HostID, "tunnel_id": b.TunnelID, "connector_id": b.ConnectorID,
		"session_id": b.SessionID, "route_id": b.RouteID,
		"edge_node_id": b.EdgeNodeID, "edge_process_epoch": b.EdgeProcessEpoch,
	} {
		if !validAttachmentID(value) {
			return fmt.Errorf("%w: invalid %s", ErrAttachmentBinding, name)
		}
	}
	if b.HostID != b.OwnerMachineID {
		return fmt.Errorf("%w: host and owner machine differ", ErrAttachmentBinding)
	}

	if b.LeaseGeneration == 0 || b.ProcessGeneration == 0 || b.ConfigGeneration == 0 || b.RouteGeneration == 0 {
		return fmt.Errorf("%w: carrier generations must be positive", ErrAttachmentBinding)
	}
	if connectorprotocol.ValidateOpaqueEpoch(b.EdgeProcessEpoch) != nil {
		return fmt.Errorf("%w: invalid edge process epoch", ErrAttachmentBinding)
	}
	if !validEdgeCarrierServerSPKISHA256(b.EdgeCarrierServerSPKISHA256) || !validEdgeCarrierServerCertificateChainPEM(b.EdgeCarrierServerCertificateChainPEM, b.EdgeCarrierServerSPKISHA256) {
		return fmt.Errorf("%w: invalid edge carrier server trust", ErrAttachmentBinding)
	}
	if !validMachineIdentityPublicKey(b.MachineIdentityPublicKey) {
		return fmt.Errorf("%w: invalid machine identity public key", ErrAttachmentBinding)
	}
	if !validMachineIdentityThumbprint(b.MachineIdentityThumbprint) {
		return fmt.Errorf("%w: invalid machine identity thumbprint", ErrAttachmentBinding)
	}
	if got := machineIdentityThumbprint(b.MachineIdentityPublicKey); got != b.MachineIdentityThumbprint {
		return fmt.Errorf("%w: machine identity thumbprint does not match public key", ErrAttachmentBinding)
	}
	return nil
}

func validEdgeCarrierServerSPKISHA256(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

func validEdgeCarrierServerCertificateChainPEM(value, pin string) bool {
	if len(value) == 0 || len(value) > 64<<10 || !validEdgeCarrierServerSPKISHA256(pin) {
		return false
	}
	rest := []byte(value)
	count := 0
	for len(bytes.TrimSpace(rest)) != 0 {
		block, remaining := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(block.Bytes) == 0 {
			return false
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return false
		}
		if count == 0 {
			digest := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
			if pin != "sha256:"+hex.EncodeToString(digest[:]) {
				return false
			}
		}
		count++
		if count > 8 {
			return false
		}
		rest = remaining
	}
	return count > 0
}

// Attachment is the server response. It deliberately mirrors only the
// secret-free previewattachment wire projection. Strict decoding below means
// a future response containing token/private-key/bearer fields is rejected,
// never silently accepted or persisted.
type Attachment struct {
	Schema string `json:"schema"`
	Kind   string `json:"kind"`
	Binding
	IdempotencyKey       string      `json:"idempotency_key"`
	RequestID            string      `json:"request_id"`
	CorrelationID        string      `json:"correlation_id"`
	RequestHash          string      `json:"request_hash"`
	Endpoint             string      `json:"endpoint"`
	Target               LeaseTarget `json:"target"`
	AccessMode           string      `json:"access_mode"`
	ConfigContentHash    string      `json:"config_content_hash"`
	EdgeEndpoints        []string    `json:"edge_endpoints"`
	AttachmentGeneration uint64      `json:"attachment_generation"`
	IssuedAt             time.Time   `json:"issued_at"`
	ExpiresAt            time.Time   `json:"expires_at"`
	State                string      `json:"state"`
	EdgeReady            bool        `json:"edge_ready"`
	OriginReady          bool        `json:"origin_ready"`
	ReadyAt              *time.Time  `json:"ready_at,omitempty"`
	ReleasedAt           *time.Time  `json:"released_at,omitempty"`
}

// CarrierAdmission is the write-only safe projection used to acquire an
// authenticated carrier. It contains identity, route, generation, and
// endpoint metadata only.
type CarrierAdmission struct {
	Schema               string    `json:"schema"`
	Kind                 string    `json:"kind"`
	Binding              Binding   `json:"binding"`
	AccessMode           string    `json:"access_mode"`
	AttachmentGeneration uint64    `json:"attachment_generation"`
	ConfigContentHash    string    `json:"config_content_hash"`
	EdgeEndpoints        []string  `json:"edge_endpoints"`
	Endpoint             string    `json:"endpoint"`
	ExpiresAt            time.Time `json:"expires_at"`
}

func (a Attachment) Validate(now time.Time) error {
	if a.Schema != PreviewTunnelSchemaV1 || a.Kind != PreviewCarrierAttachmentKind {
		return fmt.Errorf("%w: invalid attachment schema or kind", ErrAttachmentBinding)
	}
	if err := a.Binding.Validate(); err != nil {
		return err
	}
	if a.AccessMode != "public" && a.AccessMode != "private" && a.AccessMode != "team" {
		return fmt.Errorf("%w: unsupported access mode %q", ErrAttachmentBinding, a.AccessMode)
	}
	if a.IdempotencyKey != a.OperationID || !validAttachmentID(a.IdempotencyKey) || len(a.RequestID) < 3 || len(a.RequestID) > 128 || len(a.CorrelationID) < 3 || len(a.CorrelationID) > 128 || hasAttachmentControl(a.RequestID) || hasAttachmentControl(a.CorrelationID) {
		return fmt.Errorf("%w: invalid trace fields", ErrAttachmentBinding)
	}
	if len(a.RequestHash) != sha256.Size*2 {
		return fmt.Errorf("%w: invalid request hash", ErrAttachmentBinding)
	}
	if _, err := hex.DecodeString(a.RequestHash); err != nil {
		return fmt.Errorf("%w: invalid request hash encoding", ErrAttachmentBinding)
	}
	if err := validateAttachmentEndpoint(a.Endpoint, false); err != nil {
		return err
	}
	if err := validateAttachmentTarget(a.Target); err != nil {
		return err
	}
	if !validAttachmentContentHash(a.ConfigContentHash) {
		return fmt.Errorf("%w: invalid config content hash", ErrAttachmentBinding)
	}
	if len(a.EdgeEndpoints) == 0 || len(a.EdgeEndpoints) > 8 {
		return fmt.Errorf("%w: invalid edge endpoint count", ErrAttachmentBinding)
	}
	for _, endpoint := range a.EdgeEndpoints {
		if err := validateAttachmentEndpoint(endpoint, true); err != nil {
			return err
		}
	}
	if a.AttachmentGeneration == 0 || a.IssuedAt.IsZero() || a.ExpiresAt.IsZero() || !a.ExpiresAt.After(a.IssuedAt) {
		return fmt.Errorf("%w: invalid attachment lifetime", ErrAttachmentBinding)
	}
	if !now.IsZero() && !a.ExpiresAt.After(now.UTC()) && a.State != "released" && a.State != "failed" {
		return fmt.Errorf("%w: attachment expired", ErrAttachmentBinding)
	}
	switch a.State {
	case "pending", "admitted":
		if a.EdgeReady || a.OriginReady || a.ReadyAt != nil || a.ReleasedAt != nil {
			return fmt.Errorf("%w: origin cannot be ready before edge", ErrAttachmentBinding)
		}
	case "edge_ready":
		if !a.EdgeReady || a.OriginReady || a.ReadyAt != nil || a.ReleasedAt != nil {
			return fmt.Errorf("%w: invalid edge-ready state", ErrAttachmentBinding)
		}
	case "ready":
		if !a.EdgeReady || !a.OriginReady || a.ReadyAt == nil || a.ReleasedAt != nil {
			return fmt.Errorf("%w: ready requires edge and origin", ErrAttachmentBinding)
		}
	case "failed":
		if a.ReadyAt != nil || a.ReleasedAt != nil {
			return fmt.Errorf("%w: failed attachment cannot be ready or released", ErrAttachmentBinding)
		}
	case "released":
		if a.ReadyAt != nil || a.ReleasedAt == nil || a.EdgeReady || a.OriginReady {
			return fmt.Errorf("%w: released attachment must be terminal and not ready", ErrAttachmentBinding)
		}
	default:
		return fmt.Errorf("%w: unknown attachment state", ErrAttachmentBinding)
	}
	return nil
}

func (a Attachment) Admission() (CarrierAdmission, error) {
	if err := a.Validate(time.Time{}); err != nil {
		return CarrierAdmission{}, err
	}
	if a.State != "admitted" && a.State != "edge_ready" && a.State != "ready" {
		return CarrierAdmission{}, fmt.Errorf("%w: attachment is not edge-admitted", ErrAttachmentBinding)
	}
	admission := CarrierAdmission{
		Schema: PreviewTunnelSchemaV1, Kind: PreviewCarrierAttachmentKind, Binding: a.Binding,
		AccessMode:           a.AccessMode,
		AttachmentGeneration: a.AttachmentGeneration, ConfigContentHash: a.ConfigContentHash,
		EdgeEndpoints: append([]string(nil), a.EdgeEndpoints...), Endpoint: a.Endpoint, ExpiresAt: a.ExpiresAt,
	}
	if err := admission.Validate(time.Time{}); err != nil {
		return CarrierAdmission{}, err
	}
	return admission, nil
}

func (a CarrierAdmission) Validate(now time.Time) error {
	if a.Schema != PreviewTunnelSchemaV1 || a.Kind != PreviewCarrierAttachmentKind {
		return fmt.Errorf("%w: invalid admission schema or kind", ErrAttachmentBinding)
	}
	if err := a.Binding.Validate(); err != nil {
		return err
	}
	if a.AttachmentGeneration == 0 || !validAttachmentContentHash(a.ConfigContentHash) || a.ExpiresAt.IsZero() || !now.IsZero() && !a.ExpiresAt.After(now.UTC()) {
		return fmt.Errorf("%w: invalid admission lifetime or content hash", ErrAttachmentBinding)
	}
	if err := validateAttachmentEndpoint(a.Endpoint, false); err != nil {
		return err
	}
	if len(a.EdgeEndpoints) == 0 || len(a.EdgeEndpoints) > 8 {
		return fmt.Errorf("%w: invalid admission edge endpoints", ErrAttachmentBinding)
	}
	for _, endpoint := range a.EdgeEndpoints {
		if err := validateAttachmentEndpoint(endpoint, true); err != nil {
			return err
		}
	}
	return nil
}

// AttachmentHTTPError intentionally exposes only status and retryability.
// Response bodies are bounded and never included in Error, preventing server
// error text from becoming a secret exfiltration channel.
type AttachmentHTTPError struct {
	StatusCode int
	Retryable  bool
}

func (e *AttachmentHTTPError) Error() string {
	if e == nil {
		return ErrAttachmentClientRejected.Error()
	}
	return fmt.Sprintf("%s: status %d", ErrAttachmentClientRejected, e.StatusCode)
}
