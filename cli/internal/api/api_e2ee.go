package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Me is the authenticated-user payload from GET /v1/me.
type Me struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	Role        string `json:"role"`
}

type EndpointCertificateDocument struct {
	Version                int    `json:"version"`
	AccountID              string `json:"account_id"`
	KeyID                  string `json:"key_id"`
	RootFingerprint        string `json:"-"`
	EndpointID             string `json:"endpoint_id"`
	Role                   string `json:"role"`
	Generation             uint64 `json:"generation"`
	Serial                 uint64 `json:"serial"`
	IssuedAt               string `json:"issued_at"`
	ExpiresAt              string `json:"expires_at"`
	Certificate            string `json:"certificate"`
	CertificateFingerprint string `json:"certificate_fingerprint"`
}

type E2EEBootstrapInput struct {
	RootPublicKey string                      `json:"root_public_key"`
	Certificate   EndpointCertificateDocument `json:"certificate"`
}

// E2EEBootstrapResult is the result of adding one trusted endpoint signing
// key. The server returns the complete active trust set so every endpoint can
// verify certificates issued by any enrolled machine.
type E2EEBootstrapResult struct {
	KeyID       string                      `json:"key_id"`
	TrustedKeys []E2EEKey                   `json:"trusted_keys"`
	Certificate EndpointCertificateDocument `json:"certificate"`
}

// E2EEKey is public trust metadata for one independently enrolled endpoint.
// PublicKey and Fingerprint are canonical base64url and lowercase hex strings
// respectively; private key material never crosses the API boundary.
type E2EEKey struct {
	KeyID       string `json:"key_id"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	Generation  uint64 `json:"generation"`
}

type E2EERoot struct {
	Version     int       `json:"version"`
	TrustedKeys []E2EEKey `json:"trusted_keys"`
	PublicKey   string    `json:"-"`
	Fingerprint string    `json:"-"`
	Generation  uint64    `json:"-"`
}

// PeerTransportKeySet contains currently authorized machine certificate
// signers. It is separate from the account ENV root authority.
type PeerTransportKeySet struct {
	Version     int       `json:"version"`
	TrustedKeys []E2EEKey `json:"trusted_keys"`
}

type PendingEndpointIdentity struct {
	RequestID     string    `json:"request_id"`
	EndpointID    string    `json:"endpoint_id"`
	Role          string    `json:"role,omitempty"`
	State         string    `json:"state,omitempty"`
	Generation    uint64    `json:"generation"`
	QUICPublicKey string    `json:"quic_public_key"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	SafetyCode    string    `json:"safety_code"`
}

// CLIEndpointRequestInput contains only public endpoint keys. The request is
// signed later by an already paired CLI and never carries a root private key.
type CLIEndpointRequestInput struct {
	OperationID   string `json:"operation_id"`
	EndpointID    string `json:"endpoint_id"`
	Generation    uint64 `json:"generation"`
	QUICPublicKey string `json:"quic_public_key"`
}

func (c *Client) E2EERoot(ctx context.Context) (E2EERoot, error) {
	var out E2EERoot
	if err := c.doStrict(ctx, http.MethodGet, "/v1/e2ee/root", nil, &out); err != nil {
		return E2EERoot{}, err
	}
	return out, nil
}

// PeerTransportKeys is the current authorized endpoint signer directory.
// These public keys grant no authority over ENV vault data.
func (c *Client) PeerTransportKeys(ctx context.Context) (PeerTransportKeySet, error) {
	var out PeerTransportKeySet
	if err := c.doStrict(ctx, http.MethodGet, "/v1/peer-keys", nil, &out); err != nil {
		return PeerTransportKeySet{}, err
	}
	return out, nil
}

func (c *Client) PendingE2EEEndpoints(ctx context.Context) ([]PendingEndpointIdentity, error) {
	var out []PendingEndpointIdentity
	if err := c.doStrict(ctx, http.MethodGet, "/v1/e2ee/pending-endpoints", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RequestCLIEndpoint creates or exactly replays a pending existing-account
// CLI endpoint enrollment request. Unlike machine endpoint enrollment this
// route uses the authenticated CLI session and carries no machine proof.
func (c *Client) RequestCLIEndpoint(ctx context.Context, input CLIEndpointRequestInput) (PendingEndpointIdentity, error) {
	if strings.TrimSpace(input.OperationID) == "" || strings.TrimSpace(input.EndpointID) == "" || input.Generation == 0 || strings.TrimSpace(input.QUICPublicKey) == "" {
		return PendingEndpointIdentity{}, errors.New("CLI endpoint enrollment request is invalid")
	}
	var out PendingEndpointIdentity
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/e2ee/endpoint-requests", input, &out, http.Header{"Idempotency-Key": []string{input.OperationID}}); err != nil {
		return PendingEndpointIdentity{}, err
	}
	return out, nil
}

func (c *Client) RegisterEndpointCertificate(ctx context.Context, operationID string, document EndpointCertificateDocument) (EndpointCertificateDocument, error) {
	var out EndpointCertificateDocument
	path := "/v1/endpoints/" + url.PathEscape(document.EndpointID) + "/certificates/" + fmt.Sprintf("%d", document.Generation)
	if err := c.doWithHeaders(ctx, http.MethodPut, path, document, &out, http.Header{"Idempotency-Key": []string{operationID}}); err != nil {
		return EndpointCertificateDocument{}, err
	}
	return out, nil
}

func (c *Client) EndpointCertificate(ctx context.Context, endpointID string, generation uint64) (EndpointCertificateDocument, error) {
	if strings.TrimSpace(endpointID) == "" || generation == 0 {
		return EndpointCertificateDocument{}, errors.New("endpoint certificate identity is invalid")
	}
	var out EndpointCertificateDocument
	path := "/v1/endpoints/" + url.PathEscape(endpointID) + "/certificates/" + fmt.Sprintf("%d", generation)
	if err := c.doStrict(ctx, http.MethodGet, path, nil, &out); err != nil {
		return EndpointCertificateDocument{}, err
	}
	return out, nil
}

func (c *Client) BootstrapE2EE(ctx context.Context, operationID string, input E2EEBootstrapInput) (E2EEBootstrapResult, error) {
	return c.bootstrapE2EE(ctx, operationID, input, false)
}

// BootstrapE2EEFresh registers public identity material owned by the current
// authenticated CLI session. The historical method name is retained on the
// v1 client surface while all CLI enrollments use this machine-scoped ceremony.
func (c *Client) BootstrapE2EEFresh(ctx context.Context, operationID string, input E2EEBootstrapInput) (E2EEBootstrapResult, error) {
	return c.bootstrapE2EE(ctx, operationID, input, true)
}

func (c *Client) bootstrapE2EE(ctx context.Context, operationID string, input E2EEBootstrapInput, fresh bool) (E2EEBootstrapResult, error) {
	var out E2EEBootstrapResult
	headers := http.Header{"Idempotency-Key": []string{operationID}}
	if fresh {
		headers.Set("X-Paperboat-Fresh-Enrollment", "1")
	}
	if err := c.doWithHeaders(ctx, http.MethodPost, "/v1/e2ee/bootstrap", input, &out, headers); err != nil {
		return E2EEBootstrapResult{}, err
	}
	return out, nil
}
