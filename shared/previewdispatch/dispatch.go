// Package previewdispatch is the preview-dispatch wire between the control plane and the machine that owns a
// preview: the secret-free, operation-bound request, its outcome, and the one canonical request-hash input.
// Both sides sign and check RequestHash over the bytes MarshalHashInput produces, so the field order here is
// part of the protocol and is pinned by a golden vector. Validation stays per side: the server enforces
// endpoint shape and lease state, the machine binds the request to itself.
package previewdispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// LazyBinding pins activation to the current policy, installation and runtime boot.
type LazyBinding struct {
	PolicyID               string `json:"policy_id"`
	PolicyGeneration       int64  `json:"policy_generation"`
	InstallationGeneration int64  `json:"installation_generation"`
	BootID                 string `json:"boot_id"`
}

// Target is the preview's origin as the owner machine dials it.
type Target struct {
	Scheme  string `json:"scheme"`
	Address string `json:"address"`
	// HostHeader is the Host the origin receives instead of the public one (http, https and h2c targets).
	HostHeader string `json:"host_header,omitempty"`
}

// Request is the secret-free, operation-bound lease projection sent to the selected host. RequestHash
// excludes itself and covers every other field, including LeaseETag.
type Request struct {
	Lazy               *LazyBinding `json:"lazy,omitempty"`
	Schema             string       `json:"schema"`
	Kind               string       `json:"kind"`
	PreviewID          string       `json:"preview_id"`
	OperationID        string       `json:"operation_id"`
	AccountID          string       `json:"account_id"`
	ActorID            string       `json:"actor_id"`
	OwnerMachineID     string       `json:"owner_machine_id"`
	OwnerSessionID     string       `json:"owner_session_id"`
	OwnerSessionKind   string       `json:"owner_session_kind"`
	Target             Target       `json:"target"`
	AccessMode         string       `json:"access_mode"`
	Endpoint           string       `json:"endpoint"`
	LeaseDeadline      time.Time    `json:"lease_deadline"`
	UserDeadline       *time.Time   `json:"user_deadline,omitempty"`
	LeaseETag          string       `json:"lease_etag"`
	State              string       `json:"state"`
	AllocationState    string       `json:"allocation_state"`
	EdgeState          string       `json:"edge_state"`
	OriginState        string       `json:"origin_state"`
	CreatedAt          time.Time    `json:"created_at"`
	LastRenewedAt      time.Time    `json:"last_renewed_at"`
	ExpectedGeneration int64        `json:"expected_generation"`
	IdempotencyKey     string       `json:"idempotency_key"`
	RequestID          string       `json:"request_id"`
	CorrelationID      string       `json:"correlation_id"`
	RequestHash        string       `json:"request_hash"`
}

// Outcome is the only response data accepted from a host. A host may accept before readiness; the server's
// machine-auth readiness observation is the authority that completes the create operation.
type Outcome struct {
	Schema      string `json:"schema"`
	Kind        string `json:"kind"`
	PreviewID   string `json:"preview_id"`
	OperationID string `json:"operation_id"`
	State       string `json:"state"`
	Generation  int64  `json:"generation"`
}

// Normalize trims and lowercases the fields both sides compare, and copies Lazy so the caller's value is not
// shared. Hash the normalized request.
func Normalize(r Request) Request {
	r.Schema = strings.TrimSpace(r.Schema)
	r.Kind = strings.TrimSpace(r.Kind)
	r.PreviewID = strings.TrimSpace(r.PreviewID)
	r.OperationID = strings.TrimSpace(r.OperationID)
	r.AccountID = strings.TrimSpace(r.AccountID)
	r.ActorID = strings.TrimSpace(r.ActorID)
	r.OwnerMachineID = strings.TrimSpace(r.OwnerMachineID)
	r.OwnerSessionID = strings.TrimSpace(r.OwnerSessionID)
	r.Target.Scheme = strings.ToLower(strings.TrimSpace(r.Target.Scheme))
	r.Target.Address = strings.TrimSpace(r.Target.Address)
	r.Target.HostHeader = strings.ToLower(strings.TrimSpace(r.Target.HostHeader))
	r.AccessMode = strings.ToLower(strings.TrimSpace(r.AccessMode))
	r.Endpoint = strings.TrimSpace(r.Endpoint)
	r.LeaseETag = strings.TrimSpace(r.LeaseETag)
	r.IdempotencyKey = strings.TrimSpace(r.IdempotencyKey)
	r.RequestID = strings.TrimSpace(r.RequestID)
	r.CorrelationID = strings.TrimSpace(r.CorrelationID)
	if r.Lazy != nil {
		lazy := *r.Lazy
		lazy.PolicyID = strings.TrimSpace(lazy.PolicyID)
		lazy.BootID = strings.TrimSpace(lazy.BootID)
		r.Lazy = &lazy
	}
	return r
}

// MarshalHashInput is the canonical byte string RequestHash is computed over: every member but request_hash,
// in struct order, with times in UTC. Call it on a normalized, validated request.
func MarshalHashInput(r Request) ([]byte, error) {
	return json.Marshal(struct {
		Lazy               *LazyBinding `json:"lazy,omitempty"`
		Schema             string       `json:"schema"`
		Kind               string       `json:"kind"`
		PreviewID          string       `json:"preview_id"`
		OperationID        string       `json:"operation_id"`
		AccountID          string       `json:"account_id"`
		ActorID            string       `json:"actor_id"`
		OwnerMachineID     string       `json:"owner_machine_id"`
		OwnerSessionID     string       `json:"owner_session_id"`
		OwnerSessionKind   string       `json:"owner_session_kind"`
		Target             Target       `json:"target"`
		AccessMode         string       `json:"access_mode"`
		Endpoint           string       `json:"endpoint"`
		LeaseDeadline      time.Time    `json:"lease_deadline"`
		UserDeadline       *time.Time   `json:"user_deadline,omitempty"`
		LeaseETag          string       `json:"lease_etag"`
		State              string       `json:"state"`
		AllocationState    string       `json:"allocation_state"`
		EdgeState          string       `json:"edge_state"`
		OriginState        string       `json:"origin_state"`
		CreatedAt          time.Time    `json:"created_at"`
		LastRenewedAt      time.Time    `json:"last_renewed_at"`
		ExpectedGeneration int64        `json:"expected_generation"`
		IdempotencyKey     string       `json:"idempotency_key"`
		RequestID          string       `json:"request_id"`
		CorrelationID      string       `json:"correlation_id"`
	}{r.Lazy, r.Schema, r.Kind, r.PreviewID, r.OperationID, r.AccountID, r.ActorID, r.OwnerMachineID, r.OwnerSessionID, r.OwnerSessionKind, r.Target, r.AccessMode, r.Endpoint, r.LeaseDeadline.UTC(), utc(r.UserDeadline), r.LeaseETag, r.State, r.AllocationState, r.EdgeState, r.OriginState, r.CreatedAt.UTC(), r.LastRenewedAt.UTC(), r.ExpectedGeneration, r.IdempotencyKey, r.RequestID, r.CorrelationID})
}

// HashOf is the lowercase SHA-256 hex digest of the canonical hash input.
func HashOf(canonical []byte) string {
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func utc(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}
