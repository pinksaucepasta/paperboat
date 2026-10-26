package api

import (
	"context"
	"net/http"
)

type PeerNetworkRegistration struct {
	OperationID                string `json:"operation_id"`
	WireGuardPublicKey         string `json:"wireguard_public_key"`
	DiscoPublicKey             string `json:"disco_public_key"`
	ExpectedKeyGeneration      uint64 `json:"expected_key_generation"`
	QUICCertificateFingerprint string `json:"quic_certificate_fingerprint"`
}
type PeerNetworkRegistrationResult struct {
	KeyGeneration  uint64 `json:"key_generation"`
	VirtualAddress string `json:"virtual_address"`
}

func (c *Client) RegisterPeerNetwork(ctx context.Context, request PeerNetworkRegistration) (PeerNetworkRegistrationResult, error) {
	var result PeerNetworkRegistrationResult
	err := c.doRequest(ctx, http.MethodPost, c.peerNetworkPath("register"), request, &result, http.Header{"Idempotency-Key": {request.OperationID}}, true)
	return result, err
}

type PeerNetworkConfigurationResult struct {
	Configuration string   `json:"configuration"`
	CandidateSet  string   `json:"candidate_set"`
	RelayGrants   []string `json:"relay_grants"`
	// RelayDenied names why hosted relay grants were withheld ("bandwidth_quota_exhausted");
	// BillingURL is the console page for adding bandwidth. Direct connections are unaffected.
	RelayDenied string `json:"relay_denied"`
	BillingURL  string `json:"billing_url"`
	// EndpointReports says the server stores the endpoints a request reports (PLAN 3b "Direct paths without
	// a relay"); request bodies decode strictly, so a client reports them only after this is true.
	EndpointReports bool `json:"endpoint_reports"`
	// MembershipGrants says the server can issue relay grants with memberships instead of per-peer lists
	// (PLAN 3b "Relay pair authorization by membership"); a client asks for them only after this is true.
	MembershipGrants bool `json:"membership_grants"`
}

// PeerNetworkConfigurationRequest asks for the signed peer-network configuration. Endpoints are the engine's
// own candidate addresses, sent only once the server announced EndpointReports and only when they changed:
// nil omits the field (the server keeps its stored set), an empty slice clears it.
type PeerNetworkConfigurationRequest struct {
	OperationID    string                 `json:"operation_id"`
	AdmittedScopes []AdmittedNetworkScope `json:"admitted_scopes,omitempty"`
	Endpoints      *[]string              `json:"endpoints,omitempty"`
	// MembershipGrants asks for relay grants with memberships; this client verifies and uses them.
	MembershipGrants bool `json:"membership_grants,omitempty"`
}

// RelayDenial is the typed error for a refresh that withheld hosted relays, or nil.
func (r PeerNetworkConfigurationResult) RelayDenial() *APIError {
	if r.RelayDenied != "bandwidth_quota_exhausted" {
		return nil
	}
	details := map[string]any{"billing_url": r.BillingURL}
	return &APIError{Status: http.StatusConflict, Code: r.RelayDenied, Message: apiErrorMessageFor(r.RelayDenied, http.StatusConflict, details), Details: details}
}

const MaxAdmittedNetworkScopes = 128

type AdmittedNetworkScope struct {
	ResourceKind string `json:"resource_kind"`
	ResourceID   string `json:"resource_id"`
}

func (c *Client) PeerNetworkConfiguration(ctx context.Context, request PeerNetworkConfigurationRequest) (PeerNetworkConfigurationResult, error) {
	var result PeerNetworkConfigurationResult
	err := c.doRequest(ctx, http.MethodPost, c.peerNetworkPath("config"), request, &result, http.Header{"Idempotency-Key": {request.OperationID}}, true)
	return result, err
}
func (c *Client) peerNetworkPath(action string) string {
	if c.machineAuth != nil {
		return "/v1/machine-peer-network/" + action
	}
	return "/v1/peer-network/" + action
}
