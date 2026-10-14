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
}

func (c *Client) PeerNetworkConfiguration(ctx context.Context, operationID string) (PeerNetworkConfigurationResult, error) {
	var result PeerNetworkConfigurationResult
	err := c.doRequest(ctx, http.MethodPost, c.peerNetworkPath("config"), struct {
		OperationID string `json:"operation_id"`
	}{operationID}, &result, http.Header{"Idempotency-Key": {operationID}}, true)
	return result, err
}
func (c *Client) peerNetworkPath(action string) string {
	if c.machineAuth != nil {
		return "/v1/machine-peer-network/" + action
	}
	return "/v1/peer-network/" + action
}
