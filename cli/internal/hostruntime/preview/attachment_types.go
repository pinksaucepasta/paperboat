package preview

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

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
