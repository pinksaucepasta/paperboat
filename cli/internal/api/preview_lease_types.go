package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PreviewTunnelSchemaV1 is the sole wire schema used by foreground previews.
const PreviewTunnelSchemaV1 = "paperboat.preview-tunnel/v1"

// PreviewLeaseTarget is the origin address owned by a preview lease.
type PreviewLeaseTarget struct {
	Scheme  string `json:"scheme"`
	Address string `json:"address"`
}

// PreviewLease is the safe server projection of a temporary preview lease.
// It deliberately contains no carrier credential or reusable secret.
type PreviewLease struct {
	RouteID          string                 `json:"route_id,omitempty"`
	Schema           string                 `json:"schema"`
	Kind             string                 `json:"kind"`
	ID               string                 `json:"id"`
	AccountID        string                 `json:"account_id"`
	ActorID          string                 `json:"actor_id"`
	OwnerMachineID   string                 `json:"owner_machine_id"`
	OwnerSessionID   string                 `json:"owner_session_id"`
	OwnerSessionKind string                 `json:"owner_session_kind"`
	Target           PreviewLeaseTarget     `json:"target"`
	AccessMode       string                 `json:"access_mode"`
	Persistent       bool                   `json:"persistent"`
	Endpoint         string                 `json:"endpoint"`
	EndpointState    string                 `json:"endpoint_state"`
	LeaseDeadline    time.Time              `json:"lease_deadline"`
	UserDeadline     *time.Time             `json:"user_deadline"`
	State            string                 `json:"state"`
	AllocationState  string                 `json:"allocation_state"`
	EdgeState        string                 `json:"edge_state"`
	OriginState      string                 `json:"origin_state"`
	CreatedAt        time.Time              `json:"created_at"`
	LastRenewedAt    time.Time              `json:"last_renewed_at"`
	Domains          []PreviewDomainSummary `json:"domains,omitempty"`

	// CreateOperationID is transport-only metadata retained from the
	// server's create operation. It is required when a host later requests a
	// carrier attachment, but is never exposed in the preview resource JSON.
	CreateOperationID string `json:"-"`

	// ETag is transport metadata returned in the HTTP response header. It is
	// not serialized into the preview resource.
	ETag string `json:"-"`
}

// PreviewLeaseCreateRequest is the canonical POST /v1/previews payload.
// Hostname allocation is server-owned; callers cannot request a vanity name.
type PreviewLeaseCreateRequest struct {
	Workspace        string             `json:"workspace,omitempty"`
	OwnerMachineID   string             `json:"owner_machine_id"`
	OwnerSessionID   string             `json:"owner_session_id"`
	OwnerSessionKind string             `json:"owner_session_kind"`
	Target           PreviewLeaseTarget `json:"target"`
	AccessMode       string             `json:"access_mode,omitempty"`
	ExpiresAt        *time.Time         `json:"expires_at,omitempty"`
	Domains          []string           `json:"domains"`
}

// MaxPreviewDomains bounds the number of aliases that can be attached to one
// temporary preview. The server remains authoritative for domain allocation;
// this bound keeps request and read projections small and deterministic.
const MaxPreviewDomains = 8

// PreviewDomainSummary is the safe domain projection nested in a preview
// lease. A preview domain is deliberately target-discriminated: it can never
// also identify a durable tunnel route.
type PreviewDomainSummary struct {
	ID             string                   `json:"id"`
	TargetKind     string                   `json:"target_kind"`
	PreviewID      string                   `json:"preview_id"`
	Hostname       string                   `json:"hostname"`
	MatchType      string                   `json:"match_type"`
	WildcardLabels *int                     `json:"wildcard_labels,omitempty"`
	State          string                   `json:"state"`
	DNS            PreviewDomainDNS         `json:"dns"`
	Certificate    PreviewDomainCertificate `json:"certificate"`
	Generation     int64                    `json:"generation"`
	ETag           string                   `json:"etag"`
	Instructions   *PreviewDNSInstructions  `json:"instructions,omitempty"`
}

type PreviewDomainDNS struct {
	Target          string     `json:"target"`
	ObservedRecords []string   `json:"observed_records,omitempty"`
	LastCheckedAt   *time.Time `json:"last_checked_at,omitempty"`
}

type PreviewDomainCertificate struct {
	State     string         `json:"state"`
	Reference string         `json:"reference,omitempty"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
	Failure   map[string]any `json:"failure,omitempty"`
}

type PreviewDNSRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   int    `json:"ttl"`
}

// PreviewDNSInstructions contains customer-actionable records only. It never
// carries a challenge token, credential, or private key.
type PreviewDNSInstructions struct {
	Schema              string             `json:"schema"`
	Kind                string             `json:"kind"`
	TargetKind          string             `json:"target_kind"`
	PreviewID           string             `json:"preview_id"`
	DomainID            string             `json:"domain_id"`
	Hostname            string             `json:"hostname"`
	Provider            string             `json:"provider"`
	Records             []PreviewDNSRecord `json:"records"`
	CertificateStrategy string             `json:"certificate_strategy"`
	VerificationState   string             `json:"verification_state"`
	Note                string             `json:"note"`
}

// PreviewLeaseOperation is returned when lease creation is still progressing.
// Creation itself is synchronous with respect to the lease identity, so the
// client follows ResourceID and fetches the lease before starting a carrier.
type PreviewLeaseOperation struct {
	Schema        string                 `json:"schema"`
	Kind          string                 `json:"kind"`
	ID            string                 `json:"id"`
	ResourceKind  string                 `json:"resource_kind"`
	ResourceID    string                 `json:"resource_id"`
	Phase         string                 `json:"phase"`
	State         string                 `json:"state"`
	Progress      int                    `json:"progress"`
	Retrying      bool                   `json:"retrying"`
	NextRetryAt   *time.Time             `json:"next_retry_at,omitempty"`
	Error         *PreviewTunnelAPIError `json:"error,omitempty"`
	CorrelationID string                 `json:"correlation_id"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

// PreviewTunnelAPIError is the safe typed error carried by v1 operations.
type PreviewTunnelAPIError struct {
	Schema        string     `json:"schema"`
	Kind          string     `json:"kind"`
	Code          string     `json:"code"`
	Component     string     `json:"component"`
	Message       string     `json:"message"`
	Outcome       string     `json:"outcome"`
	Retryable     bool       `json:"retryable"`
	RetryAt       *time.Time `json:"retry_at,omitempty"`
	RepairAction  string     `json:"repair_action,omitempty"`
	RequestID     string     `json:"request_id"`
	CorrelationID string     `json:"correlation_id"`
}

var (
	ErrPreviewLeaseInvalid      = errors.New("invalid preview lease")
	ErrPreviewLeaseETagRequired = errors.New("preview lease ETag is required")
)

type previewLeaseCreateResponse struct {
	Lease     *PreviewLease
	Operation *PreviewLeaseOperation
}

// UnmarshalJSON accepts the v1 response union without weakening the strict
// envelope decoding used by Client.doRequestMeta.
func (r *previewLeaseCreateResponse) UnmarshalJSON(raw []byte) error {
	var discriminator struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &discriminator); err != nil {
		return err
	}
	switch discriminator.Kind {
	case "preview_lease":
		var lease PreviewLease
		if err := decodePreviewLeaseStrict(raw, &lease); err != nil {
			return err
		}
		r.Lease = &lease
	case "operation":
		var operation PreviewLeaseOperation
		if err := decodePreviewLeaseStrict(raw, &operation); err != nil {
			return err
		}
		r.Operation = &operation
	default:
		return fmt.Errorf("%w: unexpected response kind %q", ErrPreviewLeaseInvalid, discriminator.Kind)
	}
	return nil
}
