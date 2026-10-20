package tunnelenrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	jitterrand "math/rand/v2"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	"github.com/quic-go/quic-go"
)

type carrierBootstrapRequest struct {
	Schema            string `json:"schema"`
	Kind              string `json:"kind"`
	SessionID         string `json:"session_id"`
	ProcessGeneration uint64 `json:"process_generation"`
	ConfigGeneration  uint64 `json:"config_generation"`
	ConfigContentHash string `json:"config_content_hash"`
}

type carrierBootstrapNode struct {
	EdgeNodeID                string   `json:"edge_node_id"`
	EdgeProcessEpoch          string   `json:"edge_process_epoch"`
	FailureDomain             string   `json:"failure_domain"`
	Endpoints                 []string `json:"endpoints"`
	ServerSPKISHA256          string   `json:"server_spki_sha256"`
	ServerCertificateChainPEM string   `json:"server_certificate_chain_pem"`
}

type carrierBootstrapDescriptor struct {
	Schema               string                              `json:"schema"`
	Kind                 string                              `json:"kind"`
	AccountID            string                              `json:"account_id"`
	TunnelID             string                              `json:"tunnel_id"`
	ConnectorID          string                              `json:"connector_id"`
	HostID               string                              `json:"host_id"`
	StableEndpointID     string                              `json:"stable_endpoint_id"`
	SessionID            string                              `json:"session_id"`
	ProcessGeneration    uint64                              `json:"process_generation"`
	CredentialGeneration uint64                              `json:"credential_generation"`
	ConfigGeneration     uint64                              `json:"config_generation"`
	ConfigContentHash    string                              `json:"config_content_hash"`
	Carriers             []carrierBootstrapNode              `json:"carriers"`
	IngressDecisions     []connectorprotocol.IngressDecision `json:"ingress_decisions"`
	IssuedAt             time.Time                           `json:"issued_at"`
	ExpiresAt            time.Time                           `json:"expires_at"`
}

// carrierBootstrapErrorPayload mirrors only the structured error fields
// defined by the connector bootstrap endpoint. Keeping the wire type private
// prevents server diagnostics from becoming part of the success descriptor.
type carrierBootstrapErrorPayload struct {
	SupportReference string     `json:"support_reference,omitempty"`
	Schema           string     `json:"schema"`
	Kind             string     `json:"kind"`
	Code             string     `json:"code"`
	Component        string     `json:"component"`
	Message          string     `json:"message"`
	Outcome          string     `json:"outcome"`
	Retryable        *bool      `json:"retryable"`
	RetryAt          *time.Time `json:"retry_at"`
	RepairAction     string     `json:"repair_action"`
	RequestID        string     `json:"request_id"`
	CorrelationID    string     `json:"correlation_id"`
}

type carrierBootstrapErrorResponse struct {
	Error carrierBootstrapErrorPayload `json:"error"`
}

func (s *HTTPSProductionAssemblySource) carrierSessionSource(ctx context.Context, request ActivationRequest, controlIdentity ControlIdentity, hello connectorprotocol.Hello, welcome connectorprotocol.Welcome, apply tunnelmanager.ApplyRequest, signer CredentialSigner) (connector.DataCarrierSessionSource, error) {
	bootstrapRequest := carrierBootstrapRequest{Schema: carrierBootstrapSchema, Kind: "carrier_bootstrap_request", SessionID: welcome.SessionID, ProcessGeneration: hello.ProcessGeneration, ConfigGeneration: apply.Snapshot.Generation, ConfigContentHash: apply.Snapshot.ContentHash}
	body, err := json.Marshal(bootstrapRequest)
	if err != nil {
		return connector.DataCarrierSessionSource{}, ErrActivation
	}
	descriptor, err := s.fetchAvailableCarrierDescriptor(ctx, request, bootstrapRequest, body)
	if err != nil {
		return connector.DataCarrierSessionSource{}, err
	}
	if err := validateCarrierDescriptor(descriptor, s.clock.Now().UTC(), request, controlIdentity, welcome, apply); err != nil {
		s.recordCarrierDescriptorRejected(bootstrapRequest, err)
		return connector.DataCarrierSessionSource{}, err
	}
	identity := connector.DataCarrierIdentity{AccountID: descriptor.AccountID, HostID: descriptor.HostID, TunnelID: descriptor.TunnelID, ConnectorID: descriptor.ConnectorID, SessionID: descriptor.SessionID, SessionGeneration: descriptor.CredentialGeneration, ProcessGeneration: descriptor.ProcessGeneration, Generation: descriptor.ConfigGeneration}
	var endpointsMu sync.RWMutex
	endpoints := make(map[string]connector.NetworkDialerConfig, len(descriptor.Carriers))
	failureDomains := make([]string, 0, len(descriptor.Carriers))
	for _, node := range descriptor.Carriers {
		configs, err := carrierNodeEndpoints(descriptor, node, identity, s.clock.Now().UTC(), s.machineTLSCertificate)
		if err != nil {
			s.recordCarrierDescriptorRejected(bootstrapRequest, &DescriptorRejectedError{Check: "carrier_endpoint"})
			return connector.DataCarrierSessionSource{}, err
		}
		endpoints[node.EdgeNodeID+"\x00"+node.EdgeProcessEpoch] = configs
		failureDomains = append(failureDomains, node.FailureDomain)
	}
	pool := connector.DefaultDataCarrierPoolConfig()
	pool.MaximumCarriers = len(failureDomains)
	for _, node := range descriptor.Carriers {
		pool.Targets = append(pool.Targets, connector.DataCarrierTarget{EdgeID: node.EdgeNodeID, ProcessEpoch: node.EdgeProcessEpoch, FailureDomain: node.FailureDomain})
	}
	pool.EdgeID = descriptor.Carriers[0].EdgeNodeID
	pool.FailureDomains = failureDomains
	pool.Preferred = connector.HTTP3
	pool.Fallback = connector.HTTP2
	pool.SingleTransport = false
	pool.Session = connector.DataCarrierIdentity{}
	pool.TargetsExpireAt = descriptor.ExpiresAt
	pool.RefreshTargets = func(refreshCtx context.Context) ([]connector.DataCarrierTarget, time.Time, error) {
		fresh, err := s.fetchCarrierDescriptor(refreshCtx, request, body)
		if err != nil {
			return nil, time.Time{}, err
		}
		if err := validateCarrierDescriptor(fresh, s.clock.Now().UTC(), request, controlIdentity, welcome, apply); err != nil {
			return nil, time.Time{}, err
		}
		next := make(map[string]connector.NetworkDialerConfig, len(fresh.Carriers))
		targets := make([]connector.DataCarrierTarget, 0, len(fresh.Carriers))
		for _, node := range fresh.Carriers {
			endpoint, err := carrierNodeEndpoints(fresh, node, identity, s.clock.Now().UTC(), s.machineTLSCertificate)
			if err != nil {
				return nil, time.Time{}, err
			}
			next[node.EdgeNodeID+"\x00"+node.EdgeProcessEpoch] = endpoint
			targets = append(targets, connector.DataCarrierTarget{EdgeID: node.EdgeNodeID, ProcessEpoch: node.EdgeProcessEpoch, FailureDomain: node.FailureDomain})
		}
		endpointsMu.Lock()
		endpoints = next
		endpointsMu.Unlock()
		return targets, fresh.ExpiresAt, nil
	}

	dialer := connector.DataCarrierDialer(func(dialCtx context.Context, dialRequest connector.DataCarrierDialRequest) (connector.DataCarrierDialResult, error) {
		endpointsMu.RLock()
		configured, ok := endpoints[dialRequest.EdgeID+"\x00"+dialRequest.ProcessEpoch]
		endpointsMu.RUnlock()
		if !ok {
			return connector.DataCarrierDialResult{}, connector.ErrInvalidDataCarrierEndpoint
		}
		result, err := connector.NewHTTPNetworkDialer(configured)(dialCtx, dialRequest)
		if err != nil && !errors.Is(dialCtx.Err(), context.Canceled) {

			s.mu.Lock()
			recorder := s.recorder
			s.mu.Unlock()
			if recorder != nil {
				fault := errorreport.ProjectFault(dialCtx, "paperboat-daemon", "carrier_dial", "peer_connect", "transport_failed", err)
				fields := map[string]string{"operation": "carrier_dial", "error_chain": strings.Join(fault.ErrorChain, ":"), "cause": fault.Cause}
				var failure *connector.HTTPCarrierFailure
				if errors.As(err, &failure) {
					fields["reason"] = failure.Stage
					if failure.Status != 0 {
						fields["http_status"] = strconv.Itoa(failure.Status)
					}
				}
				if code := quicErrorDiagnostic(err); code != "" {
					fields["quic_error"] = code
				}
				_ = recorder.Record("tunnel", carrierDialDiagnostic(err), "warning", fields)
			}
			s.report(tunnelmanager.Observation{TunnelID: request.TunnelID, ConnectorID: request.ConnectorID,
				Code: carrierDialDiagnostic(err), Retryable: true, ObservedAt: s.clock.Now().UTC(), Err: connector.ErrDataCarrierUnavailable})
		}
		return result, err
	})
	result, err := connector.NewDataCarrierSessionSource(identity, pool, dialer)
	if err != nil {
		s.recordCarrierDescriptorRejected(bootstrapRequest, &DescriptorRejectedError{Check: "carrier_pool"})
	}
	return result, err
}

// Assignment publication can follow the authenticated config ACK. Keep that
// transient bootstrap window inside this apply operation instead of waiting
// for the next manager reconciliation. Other failures retain their owner.
func (s *HTTPSProductionAssemblySource) fetchAvailableCarrierDescriptor(
	ctx context.Context,
	request ActivationRequest,
	tuple carrierBootstrapRequest,
	body []byte,
) (carrierBootstrapDescriptor, error) {
	ctx, cancel := context.WithTimeout(ctx, s.http.Timeout)
	defer cancel()
	for attempt := 1; ; attempt++ {
		descriptor, err := s.fetchCarrierDescriptor(ctx, request, body)
		if err == nil {
			return descriptor, nil
		}
		s.recordCarrierBootstrapFailure(tuple, err)
		var failure *CarrierBootstrapError
		if !errors.As(err, &failure) || failure.StatusCode != http.StatusServiceUnavailable ||
			failure.Code != "carrier_unavailable" || !failure.Retryable || failure.RepairAction != "retry" {
			return carrierBootstrapDescriptor{}, err
		}
		delay := carrierBootstrapRetryDelay(attempt)
		if failure.RetryAt != nil {
			delay = max(delay, failure.RetryAt.Sub(s.clock.Now().UTC()))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return carrierBootstrapDescriptor{}, errors.Join(ctx.Err(), err)
		case <-timer.C:
		}
	}
}

// carrierBootstrapRetryDelay is 100 ms, 200 ms, ... doubling to 4 s, plus up to a quarter of random
// spread: a connector the server keeps refusing must not be asked once a second for the whole apply
// window, and connectors that fail together must not ask together.
func carrierBootstrapRetryDelay(attempt int) time.Duration {
	delay := min(100*time.Millisecond<<min(max(attempt, 1)-1, 6), 4*time.Second)
	return delay + time.Duration(jitterrand.Int64N(int64(delay/4)+1))
}

func (s *HTTPSProductionAssemblySource) fetchCarrierDescriptor(ctx context.Context, request ActivationRequest, body []byte) (result carrierBootstrapDescriptor, resultErr error) {
	stage := ""
	defer func() {
		if resultErr != nil && stage != "" {
			var tuple carrierBootstrapRequest
			if json.Unmarshal(body, &tuple) == nil {
				s.recordCarrierDescriptorRejected(tuple, &DescriptorRejectedError{Check: stage})
			}
		}
	}()
	operationID, err := randomID("operation")
	if err != nil {
		return carrierBootstrapDescriptor{}, err
	}
	token, err := s.auth.Token(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return carrierBootstrapDescriptor{}, ErrAuthentication
	}
	path := "/v1/tunnels/" + url.PathEscape(request.TunnelID) + "/connectors/" + url.PathEscape(request.ConnectorID) + "/carrier-bootstrap"
	endpoint := *s.base
	endpoint.Path = strings.TrimRight(s.base.Path, "/") + path
	endpoint.RawPath = ""
	proof, err := s.auth.Proof(ctx, operationID, http.MethodPost, endpoint.Path, body)
	if err != nil || len(proof) == 0 {
		return carrierBootstrapDescriptor{}, ErrAuthentication
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return carrierBootstrapDescriptor{}, err
	}
	httpRequest.Header.Set("X-Paperboat-Machine-Identity", token)
	httpRequest.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	httpRequest.Header.Set("Idempotency-Key", operationID)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := s.http.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return carrierBootstrapDescriptor{}, safeEnrollmentFailure("carrier bootstrap request was interrupted", ctx.Err(), err)
		}
		return carrierBootstrapDescriptor{}, safeEnrollmentFailure("carrier bootstrap control request is unavailable", ErrUnavailable, err)
	}
	raw, tooLarge, bodyErr := readAndCloseResponseBody(response.Body, bootstrapResponseLimit)
	if bodyErr != nil || tooLarge || len(raw) == 0 {
		return carrierBootstrapDescriptor{}, safeEnrollmentFailure("carrier bootstrap response is unavailable", ErrUnavailable, bodyErr)
	}
	if response.StatusCode != http.StatusOK {
		return carrierBootstrapDescriptor{}, classifyCarrierBootstrapError(response.StatusCode, raw)
	}
	stage = "content_type"
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return carrierBootstrapDescriptor{}, safeEnrollmentFailure("carrier bootstrap response is invalid", ErrUnavailable, err)
	}
	if contentType != "application/json" {
		return carrierBootstrapDescriptor{}, ErrUnavailable
	}
	stage = "duplicate_json"
	if err := rejectDuplicateJSON(raw); err != nil {
		return carrierBootstrapDescriptor{}, safeEnrollmentFailure("carrier bootstrap response is invalid", ErrUnavailable, err)
	}
	stage = "envelope_decode"
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return carrierBootstrapDescriptor{}, safeEnrollmentFailure("carrier bootstrap response is invalid", ErrUnavailable, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err != nil {
			return carrierBootstrapDescriptor{}, safeEnrollmentFailure("carrier bootstrap response is invalid", ErrUnavailable, err)
		}
		return carrierBootstrapDescriptor{}, ErrUnavailable
	}
	if len(envelope.Data) == 0 {
		return carrierBootstrapDescriptor{}, ErrUnavailable
	}
	stage = "descriptor_decode"
	var descriptor carrierBootstrapDescriptor
	decoder = json.NewDecoder(bytes.NewReader(envelope.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return carrierBootstrapDescriptor{}, safeEnrollmentFailure("carrier bootstrap response is invalid", ErrUnavailable, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err != nil {
			return carrierBootstrapDescriptor{}, safeEnrollmentFailure("carrier bootstrap response is invalid", ErrUnavailable, err)
		}
		return carrierBootstrapDescriptor{}, ErrUnavailable
	}
	return descriptor, nil
}

// classifyCarrierBootstrapError keeps the existing status sentinels for
// malformed or older server responses, while exposing the current structured
// connector bootstrap contract to the manager. A transient bootstrap error
// also carries ErrConnectorUnavailable so Manager reports it as a retryable
// connector condition instead of treating it as an untyped activation fault.
func classifyCarrierBootstrapError(status int, raw []byte) error {
	payload, ok := decodeCarrierBootstrapError(raw)
	if !ok {
		switch status {
		case http.StatusUnauthorized:
			return ErrAuthentication
		case http.StatusForbidden:
			return ErrForbidden
		case http.StatusConflict:
			return ErrConflict
		default:
			return ErrUnavailable
		}
	}

	structured := &CarrierBootstrapError{
		StatusCode: status, Code: payload.Code, Retryable: payload.Retryable != nil && *payload.Retryable,
		RepairAction: payload.RepairAction, RequestID: payload.RequestID, CorrelationID: payload.CorrelationID, SupportReference: payload.SupportReference,
	}
	if payload.RetryAt != nil {
		retryAt := payload.RetryAt.UTC()
		structured.RetryAt = &retryAt
	}

	var classification error
	switch status {
	case http.StatusUnauthorized:
		classification = ErrAuthentication
	case http.StatusForbidden:
		classification = ErrForbidden
	case http.StatusConflict:
		// A stale session is a recoverable control race. The next control
		// reconnect supplies a new Welcome/session tuple before retrying.
		if payload.Code == "connector_session_stale" && structured.Retryable && payload.RepairAction == "reconnect" {
			classification = errors.Join(ErrUnavailable, tunnelmanager.ErrConnectorUnavailable, tunnelmanager.ErrSessionStale)
		} else {
			classification = ErrConflict
		}
	default:
		if structured.Retryable {
			classification = errors.Join(ErrUnavailable, tunnelmanager.ErrConnectorUnavailable)
		} else {
			classification = ErrUnavailable
		}
	}
	return errors.Join(classification, structured)
}

// decodeCarrierBootstrapError validates the complete error envelope before
// exposing any of its fields. This endpoint has a finite error-code set and
// rejects unknown or duplicate fields just like the success descriptor path.
func decodeCarrierBootstrapError(raw []byte) (carrierBootstrapErrorPayload, bool) {
	if len(raw) == 0 || rejectDuplicateJSON(raw) != nil {
		return carrierBootstrapErrorPayload{}, false
	}
	var response carrierBootstrapErrorResponse
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&response) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return carrierBootstrapErrorPayload{}, false
	}
	payload := response.Error
	if payload.Schema != carrierBootstrapErrorSchema || payload.Kind != "error" || payload.Component != "control" || !knownCarrierBootstrapErrorCode(payload.Code) || strings.TrimSpace(payload.Message) == "" || len(payload.Message) > 4096 || (payload.Outcome != "unchanged" && payload.Outcome != "changed" && payload.Outcome != "uncertain") || payload.Retryable == nil || strings.TrimSpace(payload.RepairAction) == "" || len(payload.RepairAction) > 128 || strings.TrimSpace(payload.RequestID) == "" || len(payload.RequestID) > 128 || strings.TrimSpace(payload.CorrelationID) == "" || len(payload.CorrelationID) > 128 {
		return carrierBootstrapErrorPayload{}, false
	}
	return payload, true
}

func knownCarrierBootstrapErrorCode(code string) bool {
	switch code {
	case "invalid_content_type", "invalid_request", "machine_identity_required", "machine_identity_invalid", "connector_access_forbidden", "connector_session_stale", "carrier_unavailable", "connector_control_invalid", "connector_control_unavailable":
		return true
	default:
		return false
	}
}

// DescriptorRejectedError names the one check a carrier bootstrap descriptor failed. It wraps
// ErrConflict, so callers keep branching on that; Check is a finite name, never a value.
type DescriptorRejectedError struct{ Check string }

func (e *DescriptorRejectedError) Error() string { return "carrier descriptor rejected: " + e.Check }

func (e *DescriptorRejectedError) Unwrap() error { return ErrConflict }

func validateCarrierDescriptor(descriptor carrierBootstrapDescriptor, now time.Time, request ActivationRequest, identity ControlIdentity, welcome connectorprotocol.Welcome, apply tunnelmanager.ApplyRequest) error {
	checks := []struct {
		name string
		bad  bool
	}{
		{"schema", descriptor.Schema != carrierBootstrapSchema || descriptor.Kind != "carrier_bootstrap_descriptor"},
		{"account_id", descriptor.AccountID != identity.AccountID},
		{"tunnel_id", descriptor.TunnelID != request.TunnelID},
		{"connector_id", descriptor.ConnectorID != request.ConnectorID},
		{"host_id", descriptor.HostID != request.HostID},
		{"stable_endpoint_id", descriptor.StableEndpointID != request.StableEndpointID || hoststate.ValidateStableEndpointID(descriptor.StableEndpointID) != nil},
		{"session_id", descriptor.SessionID != welcome.SessionID},
		{"process_generation", descriptor.ProcessGeneration != identity.ProcessGeneration},
		{"credential_generation", descriptor.CredentialGeneration != identity.CredentialGeneration},
		{"config_generation", descriptor.ConfigGeneration != apply.Snapshot.Generation},
		{"config_content_hash", descriptor.ConfigContentHash != apply.Snapshot.ContentHash},
		{"carrier_count", len(descriptor.Carriers) == 0 || len(descriptor.Carriers) > 2},
		{"lifetime", descriptor.IssuedAt.IsZero() || descriptor.ExpiresAt.IsZero() || !descriptor.ExpiresAt.After(descriptor.IssuedAt) || descriptor.ExpiresAt.Sub(descriptor.IssuedAt) > 15*time.Second},
		{"issued_in_future", descriptor.IssuedAt.After(now.Add(connectorprotocol.MaxClockSkew))},
		{"expired", !descriptor.ExpiresAt.After(now)},
		{"ingress_count", len(descriptor.IngressDecisions) > 4096},
	}
	for _, check := range checks {
		if check.bad {
			return &DescriptorRejectedError{Check: check.name}
		}
	}
	seenNodes := map[string]bool{}
	for _, node := range descriptor.Carriers {
		if connectorprotocol.ValidateIdentifier(node.EdgeNodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(node.EdgeProcessEpoch) != nil || connectorprotocol.ValidateIdentifier(node.FailureDomain) != nil || len(node.Endpoints) != 2 || seenNodes[node.EdgeNodeID+"\x00"+node.EdgeProcessEpoch] {
			return &DescriptorRejectedError{Check: "carrier_node"}
		}
		seenNodes[node.EdgeNodeID+"\x00"+node.EdgeProcessEpoch] = true
		if _, _, _, err := parseCarrierNode(node); err != nil {
			return err
		}
	}
	for _, ingress := range descriptor.IngressDecisions {
		if ingress.Validate(now) != nil {
			return &DescriptorRejectedError{Check: "ingress_decision"}
		}
		if ingress.Binding.AccountID != descriptor.AccountID || ingress.Binding.TunnelID != descriptor.TunnelID || ingress.Binding.HostID != descriptor.HostID || ingress.ConnectorID != descriptor.ConnectorID || ingress.SessionID != descriptor.SessionID || ingress.ProcessGeneration != descriptor.ProcessGeneration || ingress.ConfigGeneration != descriptor.ConfigGeneration {
			return &DescriptorRejectedError{Check: "ingress_binding"}
		}
	}
	return nil
}

func parseCarrierNode(node carrierBootstrapNode) (map[string]*url.URL, *x509.CertPool, []*x509.Certificate, error) {
	parsedEndpoints := make(map[string]*url.URL, 2)
	for _, raw := range node.Endpoints {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "h2" && parsed.Scheme != "h3") || parsed.User != nil || parsed.Hostname() == "" || parsed.Port() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsedEndpoints[parsed.Scheme] != nil {
			return nil, nil, nil, ErrConflict
		}
		parsedEndpoints[parsed.Scheme] = parsed
	}
	if parsedEndpoints["h2"] == nil || parsedEndpoints["h3"] == nil || !strings.HasPrefix(node.ServerSPKISHA256, "sha256:") || len(node.ServerSPKISHA256) != 71 {
		return nil, nil, nil, ErrConflict
	}
	decodedSPKI, err := hex.DecodeString(strings.TrimPrefix(node.ServerSPKISHA256, "sha256:"))
	if err != nil || len(decodedSPKI) != sha256.Size || "sha256:"+hex.EncodeToString(decodedSPKI) != node.ServerSPKISHA256 {
		return nil, nil, nil, ErrConflict
	}
	if len(node.ServerCertificateChainPEM) == 0 || len(node.ServerCertificateChainPEM) > 64<<10 || !strings.HasPrefix(node.ServerCertificateChainPEM, "-----BEGIN CERTIFICATE-----") || strings.ContainsRune(node.ServerCertificateChainPEM, '\x00') {
		return nil, nil, nil, ErrConflict
	}
	pool := x509.NewCertPool()
	var certificates []*x509.Certificate
	rest := []byte(node.ServerCertificateChainPEM)
	for len(rest) > 0 {
		block, next := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, nil, nil, ErrConflict
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, nil, ErrConflict
		}
		pool.AddCert(certificate)
		certificates = append(certificates, certificate)
		rest = next
	}
	if len(certificates) == 0 {
		return nil, nil, nil, ErrConflict
	}
	return parsedEndpoints, pool, certificates, nil
}

func carrierNodeEndpoints(descriptor carrierBootstrapDescriptor, node carrierBootstrapNode, identity connector.DataCarrierIdentity, now time.Time, machineTLSCertificate func(time.Time, time.Duration, []*url.URL) (tls.Certificate, error)) (connector.NetworkDialerConfig, error) {
	parsed, roots, _, err := parseCarrierNode(node)
	if err != nil {
		return connector.NetworkDialerConfig{}, err
	}
	binding := connectorprotocol.CarrierIdentityBinding{AccountID: identity.AccountID, HostID: identity.HostID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, ConfigGeneration: identity.Generation, EdgeProcessEpoch: node.EdgeProcessEpoch}
	uri, err := connectorprotocol.CarrierIdentityURN(binding)
	if err != nil {
		return connector.NetworkDialerConfig{}, err
	}
	if machineTLSCertificate == nil {
		return connector.NetworkDialerConfig{}, ErrActivation
	}
	lifetime := descriptor.ExpiresAt.Sub(now)
	certificate, err := machineTLSCertificate(now, lifetime, []*url.URL{uri})
	if err != nil {
		return connector.NetworkDialerConfig{}, err
	}
	newEndpoint := func(parsed *url.URL) connector.DataCarrierEndpointConfig {
		tlsConfig := &tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}, ServerName: parsed.Hostname(),
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
					return connector.ErrDataCarrierTLS
				}
				digest := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
				if "sha256:"+hex.EncodeToString(digest[:]) != node.ServerSPKISHA256 {
					return connector.ErrDataCarrierTLS
				}
				return nil
			},
		}
		return connector.DataCarrierEndpointConfig{Address: parsed.Host, TLS: tlsConfig, ExpectedIdentity: identity, PeerBinding: func(tls.ConnectionState) (connector.DataCarrierIdentity, error) { return identity, nil }}
	}
	return connector.NetworkDialerConfig{TCPMux: newEndpoint(parsed["h2"]), QUIC: newEndpoint(parsed["h3"])}, nil
}

var _ ProductionAssemblySource = (*HTTPSProductionAssemblySource)(nil)

// quicErrorDiagnostic names who closed a QUIC connection and with which
// finite code, e.g. transport_remote_0x12a for a peer TLS bad_certificate
// alert. The peer's free-form error message is never included.
func quicErrorDiagnostic(err error) string {
	side := func(remote bool) string {
		if remote {
			return "remote"
		}
		return "local"
	}
	var transport *quic.TransportError
	var application *quic.ApplicationError
	var idle *quic.IdleTimeoutError
	var handshake *quic.HandshakeTimeoutError
	var reset *quic.StatelessResetError
	switch {
	case errors.As(err, &transport):
		return "transport_" + side(transport.Remote) + "_0x" + strconv.FormatUint(uint64(transport.ErrorCode), 16)
	case errors.As(err, &application):
		return "application_" + side(application.Remote) + "_0x" + strconv.FormatUint(uint64(application.ErrorCode), 16)
	case errors.As(err, &idle):
		return "idle_timeout"
	case errors.As(err, &handshake):
		return "handshake_timeout"
	case errors.As(err, &reset):
		return "stateless_reset"
	}
	return ""
}

// carrierDialDiagnostic exposes only finite error categories, never a peer's
// response, certificate content, address or credential material.
func carrierDialDiagnostic(err error) string {
	var hostname x509.HostnameError
	var root x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var network net.Error
	switch {
	case errors.As(err, &hostname):
		return "carrier_tls_hostname"
	case errors.As(err, &root):
		return "carrier_tls_root"
	case errors.As(err, &invalid):
		if invalid.Reason == x509.Expired {
			return "carrier_tls_expired"
		}
		return "carrier_tls_certificate"
	case errors.Is(err, connector.ErrHTTP3Unavailable), errors.Is(err, context.DeadlineExceeded):
		return "carrier_timeout"
	case errors.As(err, &network) && network.Timeout():
		return "carrier_timeout"
	case errors.Is(err, connector.ErrDataCarrierAdmission):
		return "carrier_admission"
	case errors.Is(err, connector.ErrInvalidDataCarrierEndpoint):
		return "carrier_endpoint"
	case errors.Is(err, connector.ErrDataCarrierTLS):
		return "carrier_tls"
	default:
		return "carrier_transport"
	}
}
