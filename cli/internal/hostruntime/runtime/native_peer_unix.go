//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=optional-authorized-relay-region
	"tailscale.com/tailcfg"
)

type productionNativePeerConfig struct {
	controlURL, issuer, stateRoot, machineID string
	generation                               uint64
	transport                                http.RoundTripper
	identity                                 managedSSHIdentitySource
	keys                                     tailnet.NetworkKeys
	authorizer                               server.AuthorizerFactory
	serve                                    func(net.Conn) error
	transfer                                 http.Handler
	ssh                                      *managedssh.Host
	privateCurrent                           server.NativePrivateTCPCurrent
	privateDial                              server.NativePrivateTCPDial
	inspector                                http.Handler
	inspectorStore                           *inspector.Store
	usage                                    *bandwidth.Recorder
}

type productionNativePeerService struct {
	config          productionNativePeerConfig
	mu              sync.Mutex
	cancel          context.CancelFunc
	current         *productionNativePeerGeneration
	done            chan struct{}
	lastErr         error
	cleanupErr      error
	startGeneration func(context.Context) (*productionNativePeerGeneration, error)
}

type productionNativePeerGeneration struct {
	cancel              context.CancelFunc
	owner               *native.Owner
	apps                *nativesession.Service
	errors              chan error
	identityFingerprint string
	authority           *tailnet.Authority
	control             tailnet.NetworkAPI
	done                sync.WaitGroup
	once                sync.Once
}

const productionNativePeerRestartDelay = time.Second
const productionNativePrivateCurrentInterval = time.Second

var errNativePeerPending = errors.New("machine endpoint certificate is pending")

type nativePeerListenerError struct{ cause error }

func (e nativePeerListenerError) Error() string         { return "native peer listener could not start" }
func (e nativePeerListenerError) Unwrap() error         { return e.cause }
func (nativePeerListenerError) DiagnosticStage() string { return "listener_bind" }
func (nativePeerListenerError) DiagnosticCode() string  { return "native_private_failed" }

type productionNativePrivateValidator func(context.Context, nativeprivate.Binding) error

func localNativePrivateValidator(target interface {
	ValidateNativePrivateTarget(nativeprivate.Binding) error
}) productionNativePrivateValidator {
	return func(_ context.Context, binding nativeprivate.Binding) error {
		return target.ValidateNativePrivateTarget(binding)
	}
}

func productionNativeCurrent(validators ...productionNativePrivateValidator) server.NativePrivateTCPCurrent {
	if len(validators) == 0 {
		return nil
	}
	validate := func(ctx context.Context, binding nativeprivate.Binding) error {
		for _, validator := range validators {
			if validator != nil && validator(ctx, binding) == nil {
				return nil
			}
		}
		return server.ErrNativePrivateBinding
	}
	return func(ctx context.Context, binding nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
		if ctx == nil || validate(ctx, binding) != nil {
			return time.Time{}, nil, server.ErrNativePrivateBinding
		}
		revoked := make(chan struct{})
		go func() {
			timer := time.NewTicker(productionNativePrivateCurrentInterval)
			expiry := time.NewTimer(time.Until(binding.ExpiresAt))
			defer timer.Stop()
			defer expiry.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-expiry.C:
					close(revoked)
					return
				case <-timer.C:
					if validate(ctx, binding) != nil {
						close(revoked)
						return
					}
				}
			}
		}()
		return binding.ExpiresAt, revoked, nil
	}
}

func productionNativePrivateDial(ctx context.Context, network, address string) (net.Conn, error) {
	if ctx == nil || network != "tcp" {
		return nil, server.ErrNativePrivateBinding
	}
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func newProductionNativePeerService(config productionNativePeerConfig) (*productionNativePeerService, error) {
	if config.controlURL == "" || config.issuer == "" || config.stateRoot == "" || config.machineID == "" || config.generation == 0 || config.transport == nil || config.identity == nil || config.keys == nil || config.authorizer == nil || config.serve == nil || config.transfer == nil {
		return nil, ErrProductionInvalid
	}
	service := &productionNativePeerService{config: config}
	service.startGeneration = service.buildGeneration
	return service, nil
}

func (s *productionNativePeerService) Start(ctx context.Context) error {
	s.mu.Lock()
	if ctx == nil || s.cancel != nil {
		s.mu.Unlock()
		return ErrProductionInvalid
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	s.lastErr = nil
	s.cleanupErr = nil
	s.mu.Unlock()
	generation, err := s.startGeneration(runCtx)
	if err != nil {
		if runCtx.Err() == nil && transientNativePeerStart(err) {
			s.recordPeerState(runCtx, err)
			// Keep initial control or approval failure under the same bounded
			// supervisor as later failures. No listener exists until admission passes.
			go s.supervise(runCtx, nil)
			return nil
		}
		cancel()
		s.mu.Lock()
		s.cancel = nil
		close(s.done)
		s.done = nil
		s.lastErr = err
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	s.current = generation
	s.mu.Unlock()
	go s.supervise(runCtx, generation)
	return nil
}

// Approval and transient control failures keep the existing supervisor alive.
// Invalid identity, protocol, or authorization still fails startup closed.
func transientNativePeerStart(err error) bool {
	if errors.Is(err, errNativePeerPending) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var response *clientapi.APIError
	if errors.As(err, &response) {
		return response.Status == 408 || response.Status == 429 || response.Status >= 500
	}
	var network net.Error
	return errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

func (s *productionNativePeerService) buildGeneration(ctx context.Context) (*productionNativePeerGeneration, error) {
	endpoint, err := runtimeEnvironmentEndpoint(s.config.stateRoot)
	if err != nil || endpoint.Generation != s.config.generation {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	if len(endpoint.Certificate) == 0 {
		// Approval is asynchronous. Keep the host runtime alive while the
		// enrollment service polls; no peer listener or relay is exposed until
		// a certificate can be verified for this exact machine generation.
		return nil, errNativePeerPending
	}
	certificate, err := endpointidentity.Verify(endpoint.Certificate, endpoint.RootPublicKey, endpointidentity.Expected{Role: endpointidentity.RoleMachine, EndpointID: s.config.machineID, Generation: endpoint.Generation}, time.Now().UTC())
	if err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	issueLeaf := func() (tls.Certificate, error) {
		now := time.Now().UTC()
		lifetime := certificate.Claims.ExpiresAt.Sub(now)
		if lifetime > 24*time.Hour {
			lifetime = 24 * time.Hour
		}
		return endpointidentity.NewTLSCertificate(certificate, endpoint.RootPublicKey, endpoint.QUICPrivateKey, now, lifetime)
	}
	leaf, err := issueLeaf()
	if err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	getCertificate := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		fresh, issueErr := issueLeaf()
		return &fresh, issueErr
	}
	getClientCertificate := func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		fresh, issueErr := issueLeaf()
		return &fresh, issueErr
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{leaf}, GetCertificate: getCertificate, GetClientCertificate: getClientCertificate} //nolint:gosec
	relayTLS := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{leaf}, GetClientCertificate: getClientCertificate}
	self := tailnet.NetworkBinding{AccountID: certificate.Claims.AccountID, EndpointID: s.config.machineID, Role: "machine", MachineID: s.config.machineID, EndpointGeneration: endpoint.Generation, MachineGeneration: s.config.generation, QUICCertificateFingerprint: certificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(endpoint.QUICPublicKey())}
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: clientconfig.ProfileStore{Path: filepath.Join(s.config.stateRoot, "peer-network"), Secrets: clientconfig.FileSecretStore{Dir: filepath.Join(s.config.stateRoot, "peer-network", "secrets")}}, Issuer: s.config.issuer, Self: self, Keys: s.config.keys})
	if err != nil {
		return nil, err
	}
	control := clientapi.New(s.config.controlURL, clientconfig.Credential{}, &http.Client{Transport: s.config.transport, Timeout: 15 * time.Second})
	control.SetMachineAuth(s.config.identity)
	request, cancelRequest := context.WithTimeout(ctx, 15*time.Second)
	err = authority.Register(request, control, false)
	cancelRequest()
	if err != nil {
		_ = authority.Close()
		return nil, err
	}
	regions, err := authority.ConfigureRegionalRelays(relayTLS)
	if err != nil {
		_ = authority.Close()
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	usage := s.config.usage
	owner, err := native.NewOwner(native.Config{Authority: authority, TLS: tlsConfig, MeterIncoming: func(conn net.Conn, binding native.MeterBinding) net.Conn {
		return usage.Wrap(conn, bandwidth.Binding{AccessSessionID: binding.AccessSessionID, StreamID: binding.StreamID, Consumer: binding.Consumer, Reverse: binding.Reverse}, func(direction string) bandwidth.Path {
			mode, node := binding.Path(direction)
			return bandwidth.Path{Mode: mode, NodeID: node}
		})
	}, Observe: func(event native.Event) {
		if event.Err == nil || ctx.Err() != nil {
			return
		}
		switch event.Kind {
		case "accept_failed":
			// These workers have no foreground caller to own their final failure.
			errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "peer_stream", "peer_connect", "native_private_failed", event.Err)
		case "serve_failed":
			errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "peer_stream", "lifecycle", "service_failed", event.Err)
		case "dial_failed":
			errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "peer_stream", "peer_connect", "native_private_failed", event.Err)
		}
	}, RefreshAuthority: func(refreshCtx context.Context) error {
		request, cancel := context.WithTimeout(refreshCtx, 15*time.Second)
		defer cancel()
		return authority.Refresh(request, control)
	}})
	if err != nil {
		_ = authority.Close()
		return nil, err
	}
	appsConfig := nativesession.Config{Authorize: s.inspectorNetworkAuthorizer(), ObserveFailure: func(serveCtx context.Context, kind nativesession.FailureKind, err error) {
		stage, code := "lifecycle", "service_failed"
		if kind == nativesession.FailureTransfer {
			stage, code = "delivery", "file_transfer_failed"
		}
		errorreport.Current().CaptureFailure(serveCtx, "paperboat-daemon", "peer_stream", stage, code, err)
	}, ServeTransfer: func(serveCtx context.Context, connection net.Conn) error {
		return server.ServeHTTPConnection(serveCtx, connection, s.config.transfer)
	}, ServeStream: s.serveStream}
	if s.config.privateCurrent != nil && s.config.privateDial != nil {
		appsConfig.ServeHTTP3 = func(serveCtx context.Context, session *native.Session, authorize func(context.Context, streamauth.Header) (string, error)) error {
			return server.ServeNativePrivateHTTP3(serveCtx, session, authorize, s.config.privateCurrent, s.config.privateDial, s.config.inspectorStore)
		}
	}
	apps, err := nativesession.New(appsConfig)
	if err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	var firstRegion *tailcfg.DERPRegion
	if len(regions) != 0 {
		firstRegion = regions[0]
	}
	if _, err := authority.Listen(firstRegion); err != nil {
		return nil, nativePeerListenerError{cause: errors.Join(err, owner.Close())}
	}
	runCtx, cancel := context.WithCancel(ctx)
	generation := &productionNativePeerGeneration{cancel: cancel, owner: owner, apps: apps, errors: make(chan error, 2), identityFingerprint: certificate.Fingerprint(), authority: authority, control: control}
	generation.done.Add(2)
	go func() {
		defer generation.done.Done()
		generation.errors <- authority.Run(runCtx, control, nil)
	}()
	go func() {
		defer generation.done.Done()
		generation.errors <- owner.Listen(runCtx, firstRegion, func(serveCtx context.Context, session *native.Session) error {
			return apps.Serve(nativePeerApplicationContext(serveCtx, ctx), session)
		})
	}()
	return generation, nil
}

// The native owner supplies its own cancellation lifetime. Lend the daemon's
// diagnostic metadata without replacing that lifetime or closing its recorder.
func nativePeerApplicationContext(serveCtx, daemonCtx context.Context) context.Context {
	if diagnostics.FromContext(serveCtx) == nil {
		if local := diagnostics.FromContext(daemonCtx); local != nil {
			serveCtx = diagnostics.WithRecorder(serveCtx, local)
		}
	}
	if supportref.FromContext(serveCtx) == "" {
		if reference := supportref.FromContext(daemonCtx); reference != "" {
			serveCtx = supportref.WithContext(serveCtx, reference)
		}
	}
	return serveCtx
}

func (g *productionNativePeerGeneration) stop() error {
	var result error
	g.once.Do(func() {
		if g.cancel != nil {
			g.cancel()
		}
		if g.owner != nil {
			result = g.owner.Close()
		}
		g.done.Wait()
		if g.apps != nil {
			g.apps.Wait()
		}

	})
	return result
}

func (s *productionNativePeerService) supervise(ctx context.Context, generation *productionNativePeerGeneration) {
	identityCheck := time.NewTicker(tailnet.RefreshInterval)
	defer identityCheck.Stop()
	defer func() {
		var cleanupErr error
		if generation != nil {
			cleanupErr = generation.stop()
		}
		s.mu.Lock()
		s.current = nil
		s.cancel = nil
		s.cleanupErr = errors.Join(s.cleanupErr, cleanupErr)
		close(s.done)
		s.mu.Unlock()
	}()
	if generation == nil {
		// Initial approval uses the same bounded retry loop as a renewed
		// certificate. Cancellation interrupts the wait and closes done.
		ticker := time.NewTicker(productionNativePeerRestartDelay)
		defer ticker.Stop()
		for generation == nil {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			next, err := s.startGeneration(ctx)
			s.recordPeerState(ctx, err)
			s.mu.Lock()
			if err == nil {
				generation = next
				s.current = next
			}
			s.mu.Unlock()
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-generation.errors:
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				err = errors.New("native peer worker stopped unexpectedly")
			}
			s.recordPeerState(ctx, err)
			stopErr := generation.stop()
			if stopErr != nil {
				errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "peer_identity", "component_shutdown", "service_failed", stopErr)
			}
			s.mu.Lock()
			s.lastErr = errors.Join(s.lastErr, stopErr)
			s.mu.Unlock()
		case <-identityCheck.C:
			fingerprint, err := s.currentIdentityFingerprint()
			if err == nil && fingerprint == generation.identityFingerprint {
				continue
			}
			if err == nil {
				err = errors.New("native endpoint identity changed")
				s.recordPeerEvent(ctx, "identity_renewed")
			} else {
				s.recordPeerState(ctx, err)
			}
			s.mu.Lock()
			s.lastErr = err
			s.mu.Unlock()
			stopErr := generation.stop()
			if stopErr != nil {
				errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "peer_identity", "component_shutdown", "service_failed", stopErr)
			}
			s.mu.Lock()
			s.lastErr = errors.Join(s.lastErr, stopErr)
			s.mu.Unlock()
		}
		timer := time.NewTimer(productionNativePeerRestartDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		for {
			next, err := s.startGeneration(ctx)
			s.recordPeerState(ctx, err)
			if err == nil {
				generation = next
				s.mu.Lock()
				s.current = next
				s.lastErr = nil
				s.mu.Unlock()
				break
			}
			timer.Reset(productionNativePeerRestartDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

// Publish transitions rather than every retry. The retained error remains the
// original typed cause; diagnostic comparison uses only bounded safe metadata.
func (s *productionNativePeerService) recordPeerState(ctx context.Context, err error) {
	s.mu.Lock()
	previous := s.lastErr
	s.lastErr = err
	s.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		if previous != nil {
			s.recordPeerEvent(ctx, "recovered")
		}
		return
	}
	if errors.Is(err, errNativePeerPending) {
		if !errors.Is(previous, errNativePeerPending) {
			s.recordPeerEvent(ctx, "approval_pending")
		}
		return
	}
	fault := errorreport.ProjectFault(ctx, "paperboat-daemon", "peer_identity", "peer_authority", "peer_authority_failed", err)
	if previous != nil && !errors.Is(previous, errNativePeerPending) {
		prior := errorreport.ProjectFault(ctx, "paperboat-daemon", "peer_identity", "peer_authority", "peer_authority_failed", previous)
		if fault.Stage == prior.Stage && fault.Code == prior.Code && fault.Cause == prior.Cause && fault.Errno == prior.Errno && fault.HTTPStatus == prior.HTTPStatus {
			return
		}
	}
	errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "peer_identity", "peer_authority", "peer_authority_failed", err)
}

func (s *productionNativePeerService) recordPeerEvent(ctx context.Context, code string) {
	outcome := "success"
	if code == "approval_pending" {
		outcome = "rejected"
	}
	errorreport.Current().Lifecycle(ctx, "access", "peer_identity", code, outcome)
	if local := diagnostics.FromContext(ctx); local != nil {
		_ = local.RecordWithSupportReference("peer_authority", code, "info", supportref.FromContext(ctx), map[string]string{
			"component": "paperboat-daemon", "operation": "peer_identity",
		})
	}
}

// LastError exposes only the typed/runtime error retained by the service. It
// never includes network configuration or endpoint key material.
func (s *productionNativePeerService) LastError() error {
	if s == nil {
		return ErrProductionInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *productionNativePeerService) currentIdentityFingerprint() (string, error) {
	endpoint, err := runtimeEnvironmentEndpoint(s.config.stateRoot)
	if err != nil || endpoint.Generation != s.config.generation {
		return "", errors.Join(ErrProductionInvalid, err)
	}
	certificate, err := endpointidentity.Verify(endpoint.Certificate, endpoint.RootPublicKey, endpointidentity.Expected{Role: endpointidentity.RoleMachine, EndpointID: s.config.machineID, Generation: endpoint.Generation}, time.Now().UTC())
	if err != nil {
		return "", errors.Join(ErrProductionInvalid, err)
	}
	return certificate.Fingerprint(), nil
}

func (s *productionNativePeerService) serveStream(ctx context.Context, header streamauth.Header, stream net.Conn) error {
	switch header.Consumer {
	case "inspector":
		return s.serveInspector(ctx, header, stream)
	case "terminal", "exec", "config_compare":
		return s.config.serve(stream)
	case "ssh":
		if s.config.ssh == nil {
			return ErrProductionInvalid
		}
		target, ok := s.config.ssh.Target()
		if !ok {
			return managedssh.ErrSSHHostStale
		}
		_, err := s.config.ssh.Serve(ctx, target.Generation, stream)
		return err
	case "private_tcp":
		if s.config.privateCurrent == nil || s.config.privateDial == nil {
			return ErrProductionInvalid
		}
		return server.ServeNativePrivateTCP(ctx, header, stream, s.config.privateCurrent, s.config.privateDial)
	default:
		return ErrProductionInvalid
	}
}

func (s *productionNativePeerService) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.done == nil {
		s.mu.Unlock()
		return nil
	}
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
		s.mu.Lock()
		err := s.cleanupErr
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func nativeNetworkAuthorizer(factory server.AuthorizerFactory) func(context.Context, streamauth.Header) (string, error) {
	authorize := server.CredentialStreamAuthorizer(factory)
	return func(ctx context.Context, header streamauth.Header) (string, error) {
		value, err := authorize(ctx, header)
		if err != nil {
			return "", err
		}
		// Signed network scopes name the verified access session. JournalBinding
		// is a separate stable hash for operation replay and is never a grant ID.
		if value.ResourceID == "" {
			return "", tailnet.ErrAdmission
		}
		return value.ResourceID, nil
	}
}
