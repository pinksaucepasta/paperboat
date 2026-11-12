package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/shared/strictjson"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/admission"
)

const maxControlDocument = 1 << 20

const maxCanonicalRouteAssignments = 4096

var (
	ErrControlInvalid       = errors.New("private control configuration is invalid")
	ErrControlUnavailable   = errors.New("private control is unavailable")
	ErrNodeObservationStale = errors.New("node observation is stale")
	// ErrRuntimeCarrierStale: the server stored the current carrier observations and some
	// reported carriers were already replaced (409 runtime_carrier_stale); the next pull
	// replaces them here, so it is an expected answer, not a control failure.
	ErrRuntimeCarrierStale = errors.New("runtime carrier observation is stale")
	// ErrCertificateNotIssuedYet: the on-demand certificate request got 409. The server has no
	// leaf to give for that host now: not one it issues (a stray name, a revoked or changed domain),
	// or one still being issued, not ready, or waiting for DNS. The handshake falls back to the
	// platform wildcard and a later handshake asks again.
	ErrCertificateNotIssuedYet = errors.New("no certificate for this host yet")
)

// expectedAnswer reports whether a failed control request is the server's answer to the request
// (a refusal, "stale", or the caller giving up) rather than control-plane unavailability. Only
// unavailability is recorded as a control failure; an answer about the edge's own identity or a
// malformed request (401 on node identity, 400) still is, because it is an edge bug.
func expectedAnswer(err error) bool {
	var refused *BrowserTerminalRefused
	for _, answer := range []error{context.Canceled, ErrNodeObservationStale, ErrRuntimeCarrierStale, ErrCertificateNotIssuedYet,
		ErrBrowserUnauthenticated, ErrBrowserDenied, ErrLazyHostOffline, ErrLazyOriginUnavailable, ErrLazyGenerationConflict,
		ErrLazyCooldown, ErrLazyCapacity, ErrPrivateAccessGrantUnauthorized, ErrPrivateAccessGrantForbidden, ErrInspectorAccessDenied} {
		if errors.Is(err, answer) {
			return true
		}
	}
	return errors.As(err, &refused)
}

// OnDemandCertificateTimeout bounds one on-demand certificate request (the TLS handshake waits
// for it; certmagic waits up to 2 min, the selector caps its wait at 30 s).
const OnDemandCertificateTimeout = 30 * time.Second

type HTTPConfig struct {
	BaseURL        string
	Credential     string
	Timeout        time.Duration
	TLS            *tls.Config
	Client         *http.Client
	ControlFailure func(context.Context, string, error)
	ControlTrace   func(context.Context, string) (string, string, func(string, string))
	// NodeReady, when set, is closed once the server has recorded this edge process as ready.
	// Requests the server refuses from an unready node (browser access, private access grants,
	// on-demand certificates) wait for it within their own context.
	NodeReady <-chan struct{}
}

type HTTPClient struct {
	base       *url.URL
	credential string
	client     *http.Client
	// onDemand sends the on-demand certificate request: the server waits there for an issuance
	// already in flight (an ACME leaf takes seconds), longer than the control timeout.
	onDemand *http.Client
	// ingress sends the ingress decisions fetch with its own, longer timeout (connectorprotocol.IngressFetchTimeout).
	ingress        *http.Client
	controlFailure func(context.Context, string, error)
	controlTrace   func(context.Context, string) (string, string, func(string, string))
	nodeReady      <-chan struct{}
	signerMu       sync.RWMutex
	signer         DistributionRequestSigner
	// routesVersion and routesHeld are the last canonical route answer; the server answers
	// "unchanged" to its version (F21-38).
	routesMu      sync.Mutex
	routesVersion string
	routesHeld    RouteSnapshot
	routesQuiet   time.Time // when the server last answered "unchanged"; zero after a changed answer
	routesGap     time.Duration
	// silent drops this process's node state reports (registration, heartbeat, route and
	// carrier observations). During an install handover two processes share one node and
	// process epoch: the old one reports until it releases, the new one from then on, so the
	// server never sees a re-registration (which resets the node to not ready) or two views.
	silent atomic.Bool
}

// SetSilent turns this process's node state reports off or on. Pulls and usage are unaffected.
func (c *HTTPClient) SetSilent(silent bool) { c.silent.Store(silent) }

// Silent reports whether this process is not the reporting one: a replacement still warming up, or the serving
// process after it released to the replacement. Failures in that window are not control outages.
func (c *HTTPClient) Silent() bool { return c.silent.Load() }

// routeQuietGap is how long an unchanged route set is served from memory before the edge asks
// again, so an idle edge makes at most one route request per gap whatever its control interval.
const routeQuietGap = 10 * time.Second

func (c *HTTPClient) routeQuietGap() time.Duration {
	if c.routesGap > 0 {
		return c.routesGap
	}
	return routeQuietGap
}

func NewHTTPClient(config HTTPConfig) (*HTTPClient, error) {
	base, err := url.Parse(config.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") || len(config.Credential) < 32 || config.Timeout <= 0 {
		return nil, ErrControlInvalid
	}
	client := config.Client
	if client == nil {
		tlsConfig := config.TLS
		if tlsConfig == nil {
			tlsConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		} else {
			tlsConfig = tlsConfig.Clone()
			if tlsConfig.MinVersion < tls.VersionTLS13 {
				tlsConfig.MinVersion = tls.VersionTLS13
			}
		}
		client = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	} else {
		cloned := *client
		client = &cloned
	}
	client.Timeout = config.Timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrControlUnavailable }
	onDemand := *client
	onDemand.Timeout = max(config.Timeout, OnDemandCertificateTimeout)
	ingress := *client
	ingress.Timeout = max(config.Timeout, connectorprotocol.IngressFetchTimeout)
	return &HTTPClient{base: base, credential: config.Credential, client: client, onDemand: &onDemand, ingress: &ingress, controlTrace: config.ControlTrace, controlFailure: config.ControlFailure, nodeReady: config.NodeReady}, nil
}

// ErrNodeNotReady: the request was not sent because the edge process is not ready yet.
var ErrNodeNotReady = errors.New("edge node is not ready yet")

// awaitNodeReady holds a request the server would refuse from an unready node; it sends nothing.
func (c *HTTPClient) awaitNodeReady(ctx context.Context) error {
	if c.nodeReady == nil {
		return nil
	}
	select {
	case <-c.nodeReady:
		return nil
	case <-ctx.Done():
		return errors.Join(ErrNodeNotReady, ctx.Err())
	}
}

// SetDistributionRequestSigner installs the node-private signer used for
// certificate pull/ack requests. The bearer remains an outer transport
// credential, while this proof binds the exact body to the registered node
// process. It is intentionally optional for the legacy control exchanges.
func (c *HTTPClient) SetDistributionRequestSigner(signer DistributionRequestSigner) error {
	if c == nil || signer == nil {
		return ErrControlInvalid
	}
	c.signerMu.Lock()
	c.signer = signer
	c.signerMu.Unlock()
	return nil
}

func (c *HTTPClient) distributionRequestSigner() DistributionRequestSigner {
	if c == nil {
		return nil
	}
	c.signerMu.RLock()
	signer := c.signer
	c.signerMu.RUnlock()
	return signer
}

func (c *HTTPClient) Current(ctx context.Context, environment, machine, connector string) (admission.Current, error) {
	request := struct {
		Environment string `json:"environment_id"`
		Machine     string `json:"machine_id"`
		Connector   string `json:"connector_id"`
	}{environment, machine, connector}
	var response struct {
		Generation uint64 `json:"connector_generation"`
		EdgePool   string `json:"edge_pool"`
		EdgeNode   string `json:"edge_node_id"`
		Revoked    bool   `json:"revoked"`
	}
	if err := c.post(ctx, "/v1/edge/assignments/current", request, &response); err != nil {
		return admission.Current{}, err
	}
	if response.Generation == 0 || response.EdgePool == "" || response.EdgeNode == "" {
		return admission.Current{}, ErrControlUnavailable
	}
	return admission.Current{Generation: response.Generation, EdgePool: response.EdgePool, EdgeNode: response.EdgeNode, Revoked: response.Revoked}, nil
}

func (c *HTTPClient) ReportUsage(ctx context.Context, report UsageReport) (UsageResult, error) {
	var result UsageResult
	wire := struct {
		Authority   string          `json:"authority_id"`
		OperationID string          `json:"operation_id"`
		Node        string          `json:"edge_node_id"`
		Epoch       string          `json:"counter_epoch"`
		Environment string          `json:"environment_id"`
		Route       string          `json:"route_id"`
		Revision    uint64          `json:"route_revision"`
		Direction   string          `json:"direction"`
		Bytes       uint64          `json:"bytes"`
		Start       time.Time       `json:"interval_start"`
		End         time.Time       `json:"interval_end"`
		Payload     json.RawMessage `json:"signed_payload,omitempty"`
	}{report.Key.Authority, report.OperationID, report.Key.Node, report.Key.Epoch, report.Key.Environment, report.Key.Route, report.Key.Revision, report.Key.Direction, report.Bytes, report.Interval[0], report.Interval[1], json.RawMessage(report.Payload)}
	if err := c.post(ctx, "/v1/edge/usage-reports", wire, &result); err != nil {
		return UsageResult{}, err
	}
	if result.Disposition != "accounted" && result.Disposition != "expired_not_billed" {
		return UsageResult{}, ErrControlUnavailable
	}
	return result, nil
}

func (c *HTTPClient) RegisterNode(ctx context.Context, registration NodeRegistration) error {
	if err := registration.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrControlInvalid, err)
	}
	if c.silent.Load() {
		return nil
	}
	return c.post(ctx, "/v1/nodes/register", registration, nil)
}
func (c *HTTPClient) Heartbeat(ctx context.Context, observation NodeObservation) (NodeAnswer, error) {
	var answer NodeAnswer
	if c.silent.Load() {
		return answer, nil
	}
	if err := c.post(ctx, "/v1/nodes/heartbeat", observation, &answer); err != nil {
		return NodeAnswer{}, err
	}
	if answer.IngressLimits != nil && answer.IngressLimits.Validate() != nil {
		return NodeAnswer{}, &RequestFailure{Path: "/v1/nodes/heartbeat", Category: "response_invalid", SupportReference: controlReference(ctx), Err: ErrControlUnavailable, Cause: edgelimits.ErrInvalid}
	}
	return answer, nil
}

type routeAssignmentWire struct {
	RouteID                    string          `json:"route_id"`
	Revision                   uint64          `json:"route_revision"`
	Environment                string          `json:"environment_id"`
	AccountID                  string          `json:"account_id"`
	HostID                     string          `json:"host_id"`
	MachineIdentityPublicKey   string          `json:"machine_identity_public_key"`
	MachineIdentityThumbprint  string          `json:"machine_identity_thumbprint"`
	TunnelID                   string          `json:"tunnel_id"`
	ResourceID                 string          `json:"resource_id,omitempty"`
	ConnectorID                string          `json:"connector_id"`
	Generation                 uint64          `json:"connector_generation"`
	ConnectorSessionID         string          `json:"connector_session_id"`
	ConnectorProcessGeneration uint64          `json:"connector_process_generation"`
	ConfigGeneration           uint64          `json:"config_generation"`
	ConfigContentHash          string          `json:"config_content_hash"`
	AssignmentID               string          `json:"assignment_id"`
	AssignmentGeneration       uint64          `json:"assignment_generation"`
	EdgeFailureDomain          string          `json:"edge_failure_domain"`
	EdgeProcessEpoch           string          `json:"edge_process_epoch"`
	NodeID                     string          `json:"edge_node_id"`
	Kind                       string          `json:"kind"`
	PublicHost                 string          `json:"public_host"`
	MatchType                  string          `json:"match_type"`
	MatchHostname              string          `json:"match_hostname"`
	WildcardSuffix             string          `json:"wildcard_suffix"`
	PathPrefix                 string          `json:"path_prefix"`
	Priority                   int             `json:"priority"`
	Protocol                   string          `json:"protocol"`
	PublicTCPListenerID        string          `json:"public_tcp_listener_id"`
	PublicTCPPort              uint16          `json:"public_tcp_port"`
	OriginScheme               string          `json:"origin_scheme"`
	AccessMode                 string          `json:"access_mode"`
	ViewerPolicyGeneration     uint64          `json:"viewer_policy_generation"`
	OriginAddress              string          `json:"origin_address"`
	PreserveHost               bool            `json:"preserve_host"`
	HostOverride               string          `json:"host_override"`
	DomainBindings             []DomainBinding `json:"domain_bindings"`
	State                      string          `json:"state"`
	DesiredState               string          `json:"desired_state"`
	ObservedState              string          `json:"observed_state"`
	Target                     struct {
		Host                       string `json:"host"`
		Port                       uint16 `json:"port"`
		Address                    string `json:"address"`
		RouteID                    string `json:"route_id"`
		ConnectorSessionID         string `json:"connector_session_id"`
		ConnectorProcessGeneration uint64 `json:"connector_process_generation"`
		ConfigGeneration           uint64 `json:"config_generation"`
	} `json:"target"`
	PreviewState  string `json:"preview_state"`
	PreviewReason string `json:"preview_reason"`
}

func (c *HTTPClient) DesiredRoutes(ctx context.Context, nodeID string) ([]RouteAssignment, error) {
	snapshot, err := c.DesiredRouteSnapshot(ctx, nodeID, "")
	if err != nil {
		return nil, err
	}
	return snapshot.Routes, nil
}

func (c *HTTPClient) DesiredRouteSnapshot(ctx context.Context, nodeID, processEpoch string) (RouteSnapshot, error) {
	if nodeID == "" {
		return RouteSnapshot{}, ErrControlInvalid
	}
	var result struct {
		Complete  *bool                 `json:"complete"`
		Unchanged bool                  `json:"unchanged"`
		Version   string                `json:"version"`
		Routes    []routeAssignmentWire `json:"routes"`
	}
	request := struct {
		NodeID       string `json:"edge_node_id"`
		ProcessEpoch string `json:"process_epoch,omitempty"`
		Version      string `json:"version,omitempty"`
	}{NodeID: nodeID, ProcessEpoch: processEpoch}
	if processEpoch != "" {
		c.routesMu.Lock()
		request.Version = c.routesVersion
		if request.Version != "" && !c.routesQuiet.IsZero() && time.Since(c.routesQuiet) < c.routeQuietGap() {
			held := c.routesHeld
			held.Routes = append([]RouteAssignment(nil), held.Routes...)
			c.routesMu.Unlock()
			return held, nil
		}
		c.routesMu.Unlock()
	}
	if err := c.post(ctx, "/v1/edge/routes/desired-state", request, &result); err != nil {
		return RouteSnapshot{}, err
	}
	if result.Unchanged {
		c.routesMu.Lock()
		defer c.routesMu.Unlock()
		if request.Version == "" || result.Version != request.Version {
			return RouteSnapshot{}, ErrControlUnavailable
		}
		c.routesQuiet = time.Now()
		held := c.routesHeld
		held.Routes = append([]RouteAssignment(nil), held.Routes...)
		return held, nil
	}
	if len(result.Routes) > maxCanonicalRouteAssignments {
		return RouteSnapshot{}, ErrControlUnavailable
	}
	// A canonical request is always a complete snapshot.  Treat a missing or
	// false completion marker as an unavailable control response rather than
	// allowing a truncated route list to revoke the last-known-good registry.
	if processEpoch != "" && (result.Complete == nil || !*result.Complete) {
		return RouteSnapshot{}, ErrControlUnavailable
	}
	complete := result.Complete != nil && *result.Complete
	routes := make([]RouteAssignment, 0, len(result.Routes))
	canonical := false
	for _, wire := range result.Routes {
		isCanonical := wire.AssignmentID != "" || wire.Kind == "tunnel_http_wss" || wire.Kind == "tunnel_tcp" || wire.Kind == "tunnel_private_tcp" || wire.ConfigContentHash != ""
		if isCanonical {
			canonical = true
		}
		originAddress := wire.OriginAddress
		if !isCanonical && originAddress == "" {
			originAddress = wire.Target.Address
		}
		matchType := wire.MatchType
		// The durable store calls its server-owned endpoint match "managed".
		// The edge matcher uses the more explicit "managed_exact" name to
		// distinguish it from customer exact and wildcard bindings. Normalize
		// that persistence vocabulary at the control boundary.
		if isCanonical && matchType == "managed" {
			matchType = "managed_exact"
		}
		assignment := RouteAssignment{
			RouteID: wire.RouteID, Revision: wire.Revision, Environment: wire.Environment, AccountID: wire.AccountID,
			HostID: wire.HostID, MachineIdentityPublicKey: wire.MachineIdentityPublicKey, MachineIdentityThumbprint: wire.MachineIdentityThumbprint,
			TunnelID: wire.TunnelID, ResourceID: wire.ResourceID, ConnectorID: wire.ConnectorID, Generation: wire.Generation,
			ConnectorSessionID: wire.ConnectorSessionID, ConnectorProcessGeneration: wire.ConnectorProcessGeneration,
			ConfigGeneration: wire.ConfigGeneration, ConfigContentHash: wire.ConfigContentHash,
			AssignmentID: wire.AssignmentID, AssignmentGeneration: wire.AssignmentGeneration,
			EdgeFailureDomain: wire.EdgeFailureDomain, EdgeProcessEpoch: wire.EdgeProcessEpoch,
			NodeID: wire.NodeID, Kind: wire.Kind, PublicHost: wire.PublicHost, MatchType: matchType,
			MatchHostname: wire.MatchHostname, WildcardSuffix: wire.WildcardSuffix, PathPrefix: wire.PathPrefix,
			Priority: wire.Priority, Protocol: wire.Protocol, OriginScheme: wire.OriginScheme, AccessMode: wire.AccessMode,
			PublicTCPListenerID: wire.PublicTCPListenerID, PublicTCPPort: wire.PublicTCPPort,
			ViewerPolicyGeneration: wire.ViewerPolicyGeneration, OriginAddress: originAddress,
			PreserveHost: wire.PreserveHost, HostOverride: wire.HostOverride, DomainBindings: append([]DomainBinding(nil), wire.DomainBindings...), State: wire.State,
			DesiredState: wire.DesiredState, ObservedState: wire.ObservedState, TargetHost: wire.Target.Host,
			TargetPort: wire.Target.Port, TargetRouteID: wire.Target.RouteID,
			TargetConnectorSessionID:  wire.Target.ConnectorSessionID,
			TargetConnectorProcessGen: wire.Target.ConnectorProcessGeneration,
			TargetConfigGeneration:    wire.Target.ConfigGeneration,
			PreviewState:              wire.PreviewState, PreviewReason: wire.PreviewReason, Canonical: isCanonical,
		}
		routes = append(routes, assignment)
	}
	// A successful complete response with an explicit process epoch is the
	// canonical desired set, even when the server includes retained legacy
	// rows in the same document. The worker filters those rows into its
	// independent legacy registry. Legacy callers omit the epoch and retain
	// their old empty behavior.
	if complete && processEpoch != "" {
		canonical = true
	}
	snapshot := RouteSnapshot{Routes: routes, Complete: complete, Canonical: canonical}
	if processEpoch != "" {
		c.routesMu.Lock()
		c.routesVersion, c.routesHeld, c.routesQuiet = result.Version, snapshot, time.Time{}
		c.routesMu.Unlock()
	}
	return snapshot, nil
}

func (c *HTTPClient) Revocations(ctx context.Context) ([]byte, error) {
	return c.trustDocument(ctx, "/v1/trust/revocations")
}

func (c *HTTPClient) JWKS(ctx context.Context) ([]byte, error) {
	return c.trustDocument(ctx, "/v1/trust/jwks")
}

func (c *HTTPClient) trustDocument(ctx context.Context, path string) (data []byte, resultErr error) {
	ctx, finish := c.startRequest(ctx, "dependency_health")
	defer func() { finish(resultErr) }()
	endpoint := c.base.ResolveReference(&url.URL{Path: path})
	var response *http.Response
	var err error
	for attempt := 1; ; attempt++ {
		request, buildErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if buildErr != nil {
			return nil, ErrControlInvalid
		}
		request.Header.Set("Authorization", "Bearer "+c.credential)
		c.applyTraceHeaders(ctx, request.Header)
		request.Header.Set("Accept", "application/json")
		if response, err = c.client.Do(request); err == nil || !resendAfterCut(ctx, attempt, err) {
			break
		}
	}
	if err != nil {
		return nil, &RequestFailure{Path: path, Category: "transport", SupportReference: controlReference(ctx), Err: ErrControlUnavailable, Cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxControlDocument))
		return nil, &RequestFailure{Path: path, Status: response.StatusCode, Category: "http_status", SupportReference: controlReference(ctx), Err: ErrControlUnavailable}
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, maxControlDocument+1))
	if err != nil {
		return nil, &RequestFailure{Path: path, Category: "transport", SupportReference: controlReference(ctx), Err: ErrControlUnavailable, Cause: err}
	}
	if len(data) == 0 || len(data) > maxControlDocument {
		return nil, ErrControlUnavailable
	}
	return data, nil
}

func (c *HTTPClient) ObserveRoutes(ctx context.Context, nodeID string, routes []RouteObservation) error {
	if len(routes) > 1000 {
		return ErrControlInvalid
	}
	if c.silent.Load() {
		return nil
	}
	return c.post(ctx, "/v1/edge/routes/observations", struct {
		NodeID string             `json:"edge_node_id"`
		Routes []RouteObservation `json:"routes"`
	}{NodeID: nodeID, Routes: routes}, nil)
}

func (c *HTTPClient) post(ctx context.Context, path string, input, output any) error {
	return c.postNode(ctx, path, "", input, output)
}

func (c *HTTPClient) postNode(ctx context.Context, path, nodeID string, input, output any) error {
	return c.postNodeWithMaximum(ctx, path, nodeID, input, output, maxControlDocument)
}

func (c *HTTPClient) postNodeWithMaximum(ctx context.Context, path, nodeID string, input, output any, maximum int) error {
	return c.postNodeWithMaximumAndIdentity(ctx, path, nodeID, "", input, output, maximum)
}

// postNodeWithMaximumAndIdentity carries the process epoch alongside the
// node header for internal node-scoped exchanges. The server must still
// derive the authenticated principal independently; these headers are only
// a binding check and are never an authority by themselves.
func (c *HTTPClient) postNodeWithMaximumAndIdentity(ctx context.Context, path, nodeID, processEpoch string, input, output any, maximum int) (resultErr error) {
	operation := "control_reconcile"
	if path == "/v1/edge/usage-reports" {
		operation = "usage_delivery"
	} else if strings.Contains(path, "carrier") {
		operation = "connector_attach"
	} else if strings.Contains(path, "/nodes/") || strings.Contains(path, "/trust/") {
		operation = "dependency_health"
	}
	ctx, finish := c.startRequest(ctx, operation)
	defer func() { finish(resultErr) }()
	if maximum <= 0 || maximum > 16<<20 {
		return ErrControlInvalid
	}
	payload, err := json.Marshal(input)
	if err != nil || len(payload) > maximum {
		return ErrControlInvalid
	}
	httpClient := c.client
	if path == CertificateDistributionRequestPath {
		httpClient = c.onDemand
	} else if path == ingressDecisionsPath {
		httpClient = c.ingress
	}
	var response *http.Response
	for attempt := 1; ; attempt++ {
		request, buildErr := c.nodeRequest(ctx, path, nodeID, processEpoch, payload)
		if buildErr != nil {
			return buildErr
		}
		response, err = httpClient.Do(request)
		// Every node request but register is a read, an idempotent state report (heartbeat,
		// observations, distribution ack), or deduplicated by the server (usage operation id,
		// on-demand issuance). Register is not resent: replaying it resets this process's
		// readiness on the server.
		if err == nil || path == "/v1/nodes/register" || !resendAfterCut(ctx, attempt, err) {
			break
		}
	}
	if err != nil {
		return &RequestFailure{Path: path, Category: "transport", SupportReference: controlReference(ctx), Err: ErrControlUnavailable, Cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, int64(maximum)))
		if path == "/v1/nodes/heartbeat" && response.StatusCode == http.StatusConflict {
			return &RequestFailure{Path: path, Status: response.StatusCode, Category: "http_status", SupportReference: controlReference(ctx), Err: ErrNodeObservationStale}
		}
		if path == runtimewire.EdgeObservePath && response.StatusCode == http.StatusConflict {
			return &RequestFailure{Path: path, Status: response.StatusCode, Category: "http_status", SupportReference: controlReference(ctx), Err: ErrRuntimeCarrierStale}
		}
		if path == CertificateDistributionRequestPath && response.StatusCode == http.StatusConflict {
			return &RequestFailure{Path: path, Status: response.StatusCode, Category: "http_status", SupportReference: controlReference(ctx), Err: ErrCertificateNotIssuedYet}
		}
		if path == InspectorAccessPath && response.StatusCode == http.StatusForbidden {
			return &RequestFailure{Path: path, Status: response.StatusCode, Category: "http_status", SupportReference: controlReference(ctx), Err: ErrInspectorAccessDenied}
		}
		return &RequestFailure{Path: path, Status: response.StatusCode, Category: "http_status", SupportReference: controlReference(ctx), Err: ErrControlUnavailable}
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, int64(maximum)+1))
		return err
	}
	limited := io.LimitReader(response.Body, int64(maximum)+1)
	data, err := io.ReadAll(limited)
	defer clearControlBytes(data)
	if err != nil {
		return &RequestFailure{Path: path, Category: "transport", SupportReference: controlReference(ctx), Err: ErrControlUnavailable, Cause: err}
	}
	if len(data) > maximum {
		return ErrControlUnavailable
	}
	if err := strictjson.DecodeResponse(data, output, 64); err != nil {
		return &RequestFailure{Path: path, Status: response.StatusCode, Category: "response_invalid", SupportReference: controlReference(ctx), Err: ErrControlUnavailable, Cause: err}
	}
	return nil
}

// nodeRequest builds one attempt of a node request. A distribution request is signed per attempt,
// so a resend carries a fresh single-use nonce, never the one the server may already have seen.
func (c *HTTPClient) nodeRequest(ctx context.Context, path, nodeID, processEpoch string, payload []byte) (*http.Request, error) {
	endpoint := c.base.ResolveReference(&url.URL{Path: path})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, ErrControlInvalid
	}
	request.Header.Set("Authorization", "Bearer "+c.credential)
	c.applyTraceHeaders(ctx, request.Header)
	if nodeID != "" {
		request.Header.Set("X-Paperboat-Edge-Node-ID", nodeID)
	}
	if processEpoch != "" {
		request.Header.Set("X-Paperboat-Edge-Process-Epoch", processEpoch)
	}
	distribution := path == CertificateDistributionPullPath || path == CertificateDistributionAckPath || path == CertificateDistributionRequestPath
	if nodeID != "" && distribution {
		signer := c.distributionRequestSigner()
		if signer == nil {
			return nil, ErrControlInvalid
		}
		proof, err := signer.SignDistributionRequest(ctx, http.MethodPost, path, payload)
		if err != nil {
			return nil, err
		}
		headers, err := distributionProofHeaders(proof)
		if err != nil {
			return nil, err
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	return request, nil
}

// controlAttempts bounds the sends of one control request whose connection a server rollout cut.
const controlAttempts = 3

// resendAfterCut reports whether an idempotent control request is sent again after attempt failed,
// and waits out the backoff first. Only a connection closed before any answer is resent: a Caddy
// reload closes its kept-alive client connections, and net/http resends a request written onto one
// only when it is replayable (transport.go shouldRetryRequest), never a signed distribution request,
// and never on a fresh connection. The backoff lets the client drop the old proxy's closed
// connections, so the resend dials the new one. Timeouts are not resent. Prior art: tailscale
// control/controlclient/auto.go rebuilds each control request after util/backoff instead of
// treating one cut as an outage.
func resendAfterCut(ctx context.Context, attempt int, err error) bool {
	if attempt >= controlAttempts || !(errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)) {
		return false
	}
	timer := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type controlTraceKey struct{}
type controlCorrelation struct{ trace, reference string }

func controlReference(ctx context.Context) string {
	correlation, _ := ctx.Value(controlTraceKey{}).(controlCorrelation)
	return correlation.reference
}

func clearControlBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// startRequest keeps the attempt callback and trace under the HTTP owner. The
// final runtime owner decides whether a failed attempt ends the service.
func (c *HTTPClient) startRequest(ctx context.Context, operation string) (context.Context, func(error)) {
	finish := func(string, string) {}
	if c.controlTrace != nil {
		trace, reference, end := c.controlTrace(ctx, operation)
		finish = end
		ctx = context.WithValue(ctx, controlTraceKey{}, controlCorrelation{trace: trace, reference: reference})
	}
	return ctx, func(err error) {
		outcome, code := "success", "ok"
		if err != nil {
			outcome, code = "failed", "unavailable"
			var status interface{ DiagnosticStatus() int }
			if errors.Is(err, context.Canceled) {
				outcome, code = "canceled", "shutdown"
			} else if errors.Is(err, ErrControlInvalid) || errors.Is(err, ErrNodeObservationStale) || errors.As(err, &status) && status.DiagnosticStatus() >= 400 && status.DiagnosticStatus() < 500 {
				outcome, code = "rejected", "invalid"
			}
			if expectedAnswer(err) {
				if outcome == "failed" {
					outcome, code = "rejected", "invalid"
				}
			} else if c.controlFailure != nil && !c.silent.Load() {
				c.controlFailure(ctx, controlReference(ctx), err)
				code = "control_request_failed"
			}
		}
		finish(outcome, code)
	}
}

func (c *HTTPClient) applyTraceHeaders(ctx context.Context, headers http.Header) {
	correlation, _ := ctx.Value(controlTraceKey{}).(controlCorrelation)
	if correlation.trace != "" {
		headers.Set("sentry-trace", correlation.trace)
	}
	if correlation.reference != "" {
		headers.Set("Support-Reference", correlation.reference)
	}
}
