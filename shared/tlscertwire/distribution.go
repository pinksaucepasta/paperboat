// Package tlscertwire is the certificate-distribution wire between the control plane and an edge: the
// message, ack, pull and request documents, their paths, the ack-status rule and the proof both sides
// compute. One definition for both modules; the proof is an HMAC over json.Marshal of the message, so the
// struct's field order is part of the contract and is pinned by a golden test.
package tlscertwire

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"time"
)

const (
	PullPath    = "/v1/edge/certificates/distributions/pull"
	AckPath     = "/v1/edge/certificates/distributions/ack"
	RequestPath = "/v1/edge/certificates/distributions/request"
)

// DistributionMessage is an internal wire message. CertificatePEM and PrivateKeyPEM are only present
// on this authenticated control transport; callers must never marshal this type into a public
// response, audit event, or log line.
type DistributionMessage struct {
	Version               int       `json:"version"`
	Action                string    `json:"action"`
	CertificateID         string    `json:"certificate_id"`
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	DomainID              string    `json:"domain_id"`
	TargetKind            string    `json:"target_kind"`
	RouteID               string    `json:"route_id"`
	PreviewID             string    `json:"preview_id"`
	PreviewGeneration     uint64    `json:"preview_generation"`
	PreviewState          string    `json:"preview_state"`
	PreviewExpiresAt      time.Time `json:"preview_expires_at"`
	Hostname              string    `json:"hostname"`
	OnDemandLeaf          bool      `json:"on_demand_leaf"`
	DomainGeneration      uint64    `json:"domain_generation"`
	CertificateGeneration uint64    `json:"certificate_generation"`
	EdgeNodeID            string    `json:"edge_node_id"`
	EdgeProcessEpoch      string    `json:"edge_process_epoch"`
	AssignmentGeneration  uint64    `json:"assignment_generation"`
	Fingerprint           string    `json:"fingerprint"`
	Issuer                string    `json:"issuer"`
	NotBefore             time.Time `json:"not_before"`
	// ExpiresAt is the short-lived transport envelope deadline.  It is
	// deliberately independent from the certificate's validity interval so a
	// queued message can never be replayed for the lifetime of a leaf.
	ExpiresAt            time.Time `json:"expires_at"`
	CertificateExpiresAt time.Time `json:"certificate_expires_at"`
	CertificatePEM       []byte    `json:"certificate_pem"`
	PrivateKeyPEM        []byte    `json:"private_key_pem"`
	IssuedAt             time.Time `json:"issued_at"`
	Proof                string    `json:"proof"`
}

// DistributionAck is the edge's answer after it has acted on a message.
type DistributionAck struct {
	Version               int       `json:"version"`
	Action                string    `json:"action"`
	CertificateID         string    `json:"certificate_id"`
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	DomainID              string    `json:"domain_id"`
	TargetKind            string    `json:"target_kind"`
	RouteID               string    `json:"route_id"`
	PreviewID             string    `json:"preview_id"`
	PreviewGeneration     uint64    `json:"preview_generation"`
	PreviewState          string    `json:"preview_state"`
	PreviewExpiresAt      time.Time `json:"preview_expires_at"`
	Hostname              string    `json:"hostname"`
	DomainGeneration      uint64    `json:"domain_generation"`
	CertificateGeneration uint64    `json:"certificate_generation"`
	EdgeNodeID            string    `json:"edge_node_id"`
	EdgeProcessEpoch      string    `json:"edge_process_epoch"`
	AssignmentGeneration  uint64    `json:"assignment_generation"`
	Fingerprint           string    `json:"fingerprint"`
	Status                string    `json:"status"`
	Code                  string    `json:"code,omitempty"`
}

type DistributionPull struct {
	EdgeNodeID       string `json:"edge_node_id"`
	EdgeProcessEpoch string `json:"edge_process_epoch"`
	Limit            int    `json:"limit,omitempty"`
	Cursor           string `json:"cursor,omitempty"`
}

// DistributionPage is the pull response. The edge treats a complete page with
// no messages as nothing to install.
type DistributionPage struct {
	Version    int                   `json:"version"`
	Complete   bool                  `json:"complete"`
	NextCursor string                `json:"next_cursor,omitempty"`
	Messages   []DistributionMessage `json:"messages"`
}

// DistributionRequest asks the control plane to issue and distribute the leaf for one SNI.
type DistributionRequest struct {
	Hostname string `json:"hostname"`
}

// AckStatusMatchesAction reports whether status is a legal answer to action ("failed" answers any).
func AckStatusMatchesAction(action, status string) bool {
	if status == "failed" {
		return true
	}
	switch action {
	case "stage":
		return status == "ready"
	case "activate":
		return status == "active"
	case "retire":
		return status == "retired"
	case "revoke":
		return status == "revoked"
	default:
		return false
	}
}

// ProofMAC is the HMAC-SHA256 of the message's canonical JSON with Proof cleared. The caller owns the
// credential's length and shape check and encodes the result.
func ProofMAC(message DistributionMessage, credential []byte) ([]byte, error) {
	message.Proof = ""
	canonical, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	defer clear(canonical)
	mac := hmac.New(sha256.New, credential)
	_, _ = mac.Write(canonical)
	return mac.Sum(nil), nil
}
