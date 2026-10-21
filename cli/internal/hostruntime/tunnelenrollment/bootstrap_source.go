package tunnelenrollment

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connectorrotation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

type HTTPSProductionAssemblySourceConfig struct {
	ControlURL            string
	StateRoot             string
	HostID                string
	Transport             http.RoundTripper
	Auth                  MachineAuth
	Clock                 connectorprotocol.Clock
	Origins               tunnelmanager.OriginProber
	OriginStreams         *tunnelmanager.OriginStreamForwarder
	Drainer               connectorprotocol.Drainer
	Renewal               connectorrotation.CredentialRenewalSource
	Report                func(tunnelmanager.Observation)
	MachineTLSCertificate func(time.Time, time.Duration, []*url.URL) (tls.Certificate, error)
	// InspectorPurge revokes daemon-local inspector captures for routes that
	// stopped forwarding. Nil disables purge notification.
	InspectorPurge func(routeIDs []string)
}

// HTTPSProductionAssemblySource is the production control/bootstrap client.
// Control authentication lives exclusively in signed Hello over WSS. Carrier
// bootstrap uses renewable machine proof and returns only bounded public
// endpoint/certificate material.
type HTTPSProductionAssemblySource struct {
	base                  *url.URL
	stateRoot             string
	hostID                string
	http                  *http.Client
	auth                  MachineAuth
	clock                 connectorprotocol.Clock
	origins               tunnelmanager.OriginProber
	originStreams         *tunnelmanager.OriginStreamForwarder
	inspectorPurge        func(routeIDs []string)
	browserIngress        tunnelmanager.IngressAuthorityFunc
	drainer               connectorprotocol.Drainer
	renewal               connectorrotation.CredentialRenewalSource
	report                func(tunnelmanager.Observation)
	machineTLSCertificate func(time.Time, time.Duration, []*url.URL) (tls.Certificate, error)
	mu                    sync.Mutex
	drainers              map[string]*assemblyDrainer
	credentials           *FileCredentialStore
	recorder              *diagnostics.Recorder
	rotations             map[string]*productionRotationRuntime
	lifetime              context.Context
	cancel                context.CancelFunc
	started               bool
	closed                bool
}

func NewHTTPSProductionAssemblySource(config HTTPSProductionAssemblySourceConfig) (*HTTPSProductionAssemblySource, error) {
	base, err := url.Parse(strings.TrimSpace(config.ControlURL))
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || !filepath.IsAbs(config.StateRoot) || filepath.Clean(config.StateRoot) != config.StateRoot || connectorprotocol.ValidateIdentifier(config.HostID) != nil || config.Auth == nil || config.Clock == nil || config.Origins == nil || config.MachineTLSCertificate == nil {
		return nil, ErrInvalid
	}
	browserIngress, err := tunnelmanager.NewBrowserIngressAuthority(config.ControlURL, config.Auth, config.Transport)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: errorreport.TransportOperation(config.Transport, base.String(), "tunnel_bootstrap"), Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrInvalid }}
	if config.Report == nil {
		config.Report = func(tunnelmanager.Observation) {}
	}
	return &HTTPSProductionAssemblySource{base: base, stateRoot: config.StateRoot, hostID: config.HostID, http: client, auth: config.Auth, clock: config.Clock, origins: config.Origins, originStreams: config.OriginStreams, inspectorPurge: config.InspectorPurge, browserIngress: browserIngress, drainer: config.Drainer, renewal: config.Renewal, report: config.Report, machineTLSCertificate: config.MachineTLSCertificate, drainers: make(map[string]*assemblyDrainer), rotations: make(map[string]*productionRotationRuntime)}, nil
}

func (s *HTTPSProductionAssemblySource) BindCredentialStore(store *FileCredentialStore) error {
	if s == nil || store == nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.credentials != nil && s.credentials != store || s.started {
		return ErrConflict
	}
	s.credentials = store
	return nil
}

func (s *HTTPSProductionAssemblySource) Start(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.credentials == nil {
		return ErrUnavailable
	}
	if s.started {
		return nil
	}
	s.lifetime, s.cancel = context.WithCancel(context.Background())
	s.recorder = diagnostics.FromContext(ctx)
	s.started = true
	return nil
}

func (s *HTTPSProductionAssemblySource) Shutdown(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrInvalid
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	rotations := make([]*productionRotationRuntime, 0, len(s.rotations))
	for _, runtime := range s.rotations {
		rotations = append(rotations, runtime)
	}
	s.mu.Unlock()
	var result error
	for _, runtime := range rotations {
		result = errors.Join(result, runtime.shutdown(ctx))
	}
	return result
}

func (s *HTTPSProductionAssemblySource) ResolveProductionAssembly(ctx context.Context, request ActivationRequest, signer CredentialSigner) (tunnelmanager.ProductionAssemblyConfig, error) {
	return s.resolveProductionAssembly(ctx, request, signer, nil)
}

func (s *HTTPSProductionAssemblySource) resolveProductionAssembly(ctx context.Context, request ActivationRequest, signer CredentialSigner, rotationOverride *productionRotationRuntime) (tunnelmanager.ProductionAssemblyConfig, error) {
	if s == nil || ctx == nil || signer == nil || request.HostID != s.hostID || !validActivationRequest(request) || hoststate.ValidateStableEndpointID(request.StableEndpointID) != nil {
		return tunnelmanager.ProductionAssemblyConfig{}, ErrInvalid
	}
	identity := ControlIdentity{AccountID: request.AccountID, ProcessGeneration: request.ProcessGeneration, CredentialGeneration: request.CredentialGeneration}
	rotation := rotationOverride
	if rotation == nil {
		var err error
		rotation, err = s.rotationFor(request)
		if err != nil {
			return tunnelmanager.ProductionAssemblyConfig{}, err
		}
	}
	drainer := s.drainer
	if drainer == nil {
		drainer = s.drainerFor(request)
	}
	renewalSigner := connectorrotation.RenewalProofSignerFunc(func(signCtx context.Context, payload []byte) ([]byte, error) {
		return signer(signCtx, payload)
	})
	hello, err := s.newHello(ctx, request, identity, signer)
	if err != nil {
		return tunnelmanager.ProductionAssemblyConfig{}, err
	}
	controlStream := func(streamCtx context.Context) (io.ReadWriteCloser, error) {
		return s.openControlStream(streamCtx, request)
	}
	originStreamsFor := func(apply tunnelmanager.ApplyRequest, carrierIdentity connector.DataCarrierIdentity) *tunnelmanager.OriginStreamForwarder {
		if s.originStreams == nil {
			return nil
		}
		if carrierIdentity.AccountID != request.AccountID || carrierIdentity.TunnelID != request.TunnelID || carrierIdentity.ConnectorID != request.ConnectorID || carrierIdentity.HostID != request.HostID || carrierIdentity.ProcessGeneration == 0 || carrierIdentity.Generation != apply.Snapshot.Generation {
			return nil
		}
		candidateRequest := request
		candidateRequest.ProcessGeneration = carrierIdentity.ProcessGeneration
		ingress := &assemblyIngressAuthority{source: s, request: candidateRequest, generation: apply.Snapshot.Generation, contentHash: apply.Snapshot.ContentHash}
		forwarder := *s.originStreams
		forwarder.IngressAuthority = ingress.lookup
		return &forwarder
	}
	descriptors := func(descriptorCtx context.Context, currentHello connectorprotocol.Hello, welcome connectorprotocol.Welcome, apply tunnelmanager.ApplyRequest) (connector.DataCarrierSessionSource, error) {
		currentRequest := request
		currentRequest.ProcessGeneration = currentHello.ProcessGeneration
		currentIdentity := identity
		currentIdentity.ProcessGeneration = currentHello.ProcessGeneration
		return s.carrierSessionSource(descriptorCtx, currentRequest, currentIdentity, currentHello, welcome, apply, signer)
	}
	controlFactory := func(factoryCtx context.Context, _ *tunnelmanager.CoordinatedConfigApplier) (connectorrotation.ControlSessionConfig, error) {
		fresh, freshErr := s.newHello(factoryCtx, request, identity, signer)
		if freshErr != nil {
			return connectorrotation.ControlSessionConfig{}, freshErr
		}
		currentRotation := rotation
		if rotationOverride != nil && rotationOverride.isCommitted() {
			currentRotation, freshErr = s.rotationFor(request)
			if freshErr != nil {
				return connectorrotation.ControlSessionConfig{}, freshErr
			}
		}
		return connectorrotation.ControlSessionConfig{Hello: fresh, Drainer: drainer, Rotation: currentRotation.manager, Readiness: currentRotation, AutomaticRotationReadiness: true, RotationRevokeCommitter: currentRotation, Renewal: s.renewal, RenewalSigner: renewalSignerIfNeeded(s.renewal, renewalSigner), Clock: s.clock}, nil
	}
	return tunnelmanager.ProductionAssemblyConfig{
		Production: tunnelmanager.ProductionConfig{
			StateRoot: filepath.Join(s.stateRoot, "tunnel-connectors", request.ConnectorID, "process-"+strconv.FormatUint(request.ProcessGeneration, 10)), HostID: request.HostID,
			Report: s.report, InspectorPurge: s.inspectorPurge,
		},
		StableEndpointID: request.StableEndpointID,
		Clock:            s.clock, Origins: s.origins, OriginStreamsForGeneration: originStreamsFor, OriginHealth: s,
		CarrierDescriptorSource: descriptors,
		InitialConnector: &hoststate.Connector{
			ID: request.ConnectorID, TunnelID: request.TunnelID, HostID: request.HostID,
			Credential:         hoststate.CredentialReference{Reference: request.CredentialReference, Generation: identity.CredentialGeneration},
			RotationGeneration: identity.CredentialGeneration,
		},
		Control:               connectorrotation.ControlSessionConfig{Hello: hello, Drainer: drainer, Rotation: rotation.manager, Readiness: rotation, AutomaticRotationReadiness: true, RotationRevokeCommitter: rotation, Renewal: s.renewal, RenewalSigner: renewalSignerIfNeeded(s.renewal, renewalSigner), Clock: s.clock},
		ControlSessionFactory: controlFactory,
		ControlStream:         controlStream,
		HelloRequestID:        request.OperationID,
	}, nil
}

func (s *HTTPSProductionAssemblySource) bindReplacementProductionAssembly(request ActivationRequest, assembly *tunnelmanager.ProductionAssembly, rotation *productionRotationRuntime) error {
	if s == nil || assembly == nil || rotation == nil || !validActivationRequest(request) {
		return ErrActivation
	}
	if s.drainer == nil {
		if err := s.drainerFor(request).bind(assembly); err != nil {
			return err
		}
	}
	return nil
}

func renewalSignerIfNeeded(source connectorrotation.CredentialRenewalSource, signer connectorrotation.RenewalProofSigner) connectorrotation.RenewalProofSigner {
	if source != nil {
		return nil
	}
	return signer
}

func activationBindingKey(request ActivationRequest) string {
	return request.AccountID + "\x00" + request.TunnelID + "\x00" + request.ConnectorID + "\x00" + request.StableEndpointID + "\x00" + strconv.FormatUint(request.ProcessGeneration, 10) + "\x00" + strconv.FormatUint(request.CredentialGeneration, 10)
}

func (s *HTTPSProductionAssemblySource) drainerFor(request ActivationRequest) *assemblyDrainer {
	key := activationBindingKey(request)
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.drainers[key]; current != nil {
		return current
	}
	created := newAssemblyDrainer(request.TunnelID, request.ConnectorID)
	s.drainers[key] = created
	return created
}

func (s *HTTPSProductionAssemblySource) BindProductionAssembly(request ActivationRequest, assembly *tunnelmanager.ProductionAssembly) error {
	if s == nil || assembly == nil || !validActivationRequest(request) {
		return ErrActivation
	}
	if s.drainer == nil {
		if err := s.drainerFor(request).bind(assembly); err != nil {
			return err
		}
	}
	rotation, err := s.rotationFor(request)
	if err != nil {
		return err
	}
	return rotation.bind(assembly)
}

// UnbindProductionAssembly releases only the failed assembly instance. The
// credential, rotation journal and exact process-generation claim survive a
// retry of the same durable connector.
func (s *HTTPSProductionAssemblySource) UnbindProductionAssembly(request ActivationRequest, assembly *tunnelmanager.ProductionAssembly) {
	if s == nil || assembly == nil {
		return
	}
	key := activationBindingKey(request)
	s.mu.Lock()
	drainer, rotation := s.drainers[key], s.rotations[key]
	s.mu.Unlock()
	if drainer != nil {
		drainer.unbind(assembly)
	}
	if rotation != nil {
		rotation.unbind(assembly)
	}
}

func (s *HTTPSProductionAssemblySource) newHello(ctx context.Context, request ActivationRequest, identity ControlIdentity, signer CredentialSigner) (connectorprotocol.Hello, error) {
	now := s.clock.Now().UTC()
	nonce, err := randomNonce("control-auth")
	if err != nil {
		return connectorprotocol.Hello{}, err
	}
	auth := connectorprotocol.AuthRequest{
		AccountID: identity.AccountID, TunnelID: request.TunnelID, ConnectorID: request.ConnectorID, HostID: request.HostID,
		IdentityKeyID: request.CredentialKeyID, IdentityKeyThumbprint: request.CredentialThumbprint,
		ProcessGeneration: identity.ProcessGeneration, CredentialGeneration: identity.CredentialGeneration,
		Nonce: nonce, IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	var signErr error
	auth, err = connectorprotocol.SignAuthProof(auth, func(payload []byte) []byte {
		var signature []byte
		signature, signErr = signer(ctx, payload)
		return signature
	})
	if err != nil || signErr != nil {
		return connectorprotocol.Hello{}, errors.Join(ErrActivation, err, signErr)
	}
	hello := connectorprotocol.Hello{
		Protocol: connectorprotocol.ProtocolName, MinVersion: connectorprotocol.ProtocolVersion, MaxVersion: connectorprotocol.ProtocolVersion,
		AccountID: identity.AccountID, TunnelID: request.TunnelID, ConnectorID: request.ConnectorID, HostID: request.HostID,
		ProcessGeneration: identity.ProcessGeneration,
		Capabilities:      connectorprotocol.ProductionCapabilities(),
		Auth:              auth,
	}
	if err := hello.Validate(now); err != nil {
		return connectorprotocol.Hello{}, errors.Join(ErrActivation, err)
	}
	return hello, nil
}

func (s *HTTPSProductionAssemblySource) openControlStream(ctx context.Context, request ActivationRequest) (io.ReadWriteCloser, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	endpoint := *s.base
	endpoint.Scheme = "wss"
	endpoint.Path = strings.TrimRight(s.base.Path, "/") + "/v1/tunnels/" + url.PathEscape(request.TunnelID) + "/connectors/" + url.PathEscape(request.ConnectorID) + "/control"
	endpoint.RawPath = ""
	websocketClient := *s.http
	// The stream lifetime is owned by stable hostd's context. An ordinary HTTP
	// response timeout would tear down a healthy long-lived control session.
	websocketClient.Timeout = 0
	closeCapture := &responseBodyCloseCapture{}
	if websocketClient.Transport == nil {
		websocketClient.Transport = http.DefaultTransport
	}
	websocketClient.Transport = closeCapturingTransport{base: websocketClient.Transport, capture: closeCapture}
	// Bound only the initial handshake. Passing the stable lifetime context to
	// websocket.Dial directly lets a dead/unroutable control endpoint keep
	// activation stuck forever, leaving the enrollment manager's activating
	// fence set and making every subsequent CLI retry look unavailable. The
	// returned stream is still bound to ctx below, so a healthy WebSocket can
	// live for the lifetime of hostd.
	dialCtx, cancel := context.WithTimeout(ctx, controlDialTimeout)
	defer cancel()
	connection, response, err := websocket.Dial(dialCtx, endpoint.String(), &websocket.DialOptions{HTTPClient: &websocketClient, Subprotocols: []string{controlSubprotocol}, CompressionMode: websocket.CompressionDisabled})
	responseClosed, responseCloseErr := closeCapture.result()
	if response != nil && response.Body != nil && !responseClosed {
		responseCloseErr = response.Body.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, safeEnrollmentFailure("tunnel control stream setup was interrupted", ctx.Err(), err, responseCloseErr)
		}
		if response != nil && response.StatusCode == http.StatusGone {
			// The tunnel was deleted or the connector revoked: terminal, never retried.
			return nil, safeEnrollmentFailure("tunnel connector no longer exists", tunnelmanager.ErrConnectorGone, &ActivationDiagnostic{Code: ActivationDiagnosticControlHTTPUnavailable, Cause: err}, responseCloseErr)
		}
		if response != nil {
			code := ActivationDiagnosticControlHTTPUnavailable
			switch response.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				code = ActivationDiagnosticControlHTTPDenied
			}
			diagnostic := &ActivationDiagnostic{Code: code, Cause: err}
			return nil, safeEnrollmentFailure("tunnel control stream is unavailable", ErrUnavailable, diagnostic, responseCloseErr)
		}
		diagnostic := &ActivationDiagnostic{Code: ActivationDiagnosticControlNetworkTLS, Cause: err}
		return nil, safeEnrollmentFailure("tunnel control stream is unavailable", ErrUnavailable, diagnostic, responseCloseErr)
	}
	if responseCloseErr != nil {
		closeErr := connection.Close(websocket.StatusInternalError, "control handshake cleanup failed")
		diagnostic := &ActivationDiagnostic{Code: ActivationDiagnosticControlNetworkTLS, Cause: responseCloseErr}
		return nil, safeEnrollmentFailure("tunnel control stream cleanup failed", ErrUnavailable, diagnostic, closeErr)
	}
	if connection.Subprotocol() != controlSubprotocol {
		closeErr := connection.Close(websocket.StatusPolicyViolation, "connector subprotocol required")
		diagnostic := &ActivationDiagnostic{Code: ActivationDiagnosticInvalidSessionConfig}
		return nil, safeEnrollmentFailure("tunnel control stream configuration is invalid", ErrUnavailable, diagnostic, closeErr)
	}
	connection.SetReadLimit(connectorprotocol.MaxFrameBytes + 4)
	return websocket.NetConn(ctx, connection, websocket.MessageBinary), nil
}
