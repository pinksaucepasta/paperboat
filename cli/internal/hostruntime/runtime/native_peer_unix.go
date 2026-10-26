//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	peeridentityenrollment "github.com/pinksaucepasta/paperboat/internal/hostruntime/peeridentity"
	"github.com/pinksaucepasta/paperboat/shared/admission"
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
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
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
	dispatch                                 *server.NativeDispatchClient
	privateCurrent                           server.NativePrivateTCPCurrent
	privateDial                              server.NativePrivateTCPDial
	inspector                                http.Handler
	inspectorStore                           *inspector.Store
	usage                                    *bandwidth.Recorder
	// offlineAudit records sessions started or refused without Paperboat (F21-11); nil refuses them.
	offlineAudit *offlineaudit.Queue
	// networkEvents wakes the signed-network renewal when the server reports a peer or key change
	// (peer_network) or a machine joining or leaving (inventory): a newly paired machine enters the mesh
	// when it first comes online, which moves only inventory. nil leaves the periodic renewal alone.
	networkEvents *machineevents.Subscription
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
	// requestRefresh renews the signed network now; nil in generations built without an authority.
	requestRefresh func()
	control        tailnet.NetworkAPI
	done           sync.WaitGroup
	once           sync.Once
}

const productionNativePeerRestartDelay = time.Second

// productionNativePeerMaxRestartDelay bounds the wait between failing generation starts.
const productionNativePeerMaxRestartDelay = time.Minute

// nativePeerRestartDelay is the wait before the next start after failures consecutive failed
// starts: the base delay, doubling to the maximum, so a machine whose identity was revoked does
// not ask the server every second forever.
func nativePeerRestartDelay(failures int) time.Duration {
	if failures >= 6 {
		return productionNativePeerMaxRestartDelay
	}
	return min(productionNativePeerRestartDelay<<failures, productionNativePeerMaxRestartDelay)
}

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
		if err := target.ValidateNativePrivateTarget(binding); err != nil {
			return errors.Join(server.ErrNativePrivateBinding, err)
		}
		return nil
	}
}

func productionNativeCurrent(validators ...productionNativePrivateValidator) server.NativePrivateTCPCurrent {
	if len(validators) == 0 {
		return nil
	}
	validate := func(ctx context.Context, binding nativeprivate.Binding) error {
		var unavailable error
		for _, validator := range validators {
			if validator == nil {
				continue
			}
			err := validator(ctx, binding)
			if err == nil {
				return nil
			}
			if !errors.Is(err, server.ErrNativePrivateBinding) && !errors.Is(err, admission.ErrRevoked) {
				unavailable = errors.Join(unavailable, err)
			}
		}
		if unavailable != nil {
			return unavailable
		}
		return server.ErrNativePrivateBinding
	}
	return func(ctx context.Context, binding nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
		if ctx == nil {
			return time.Time{}, nil, server.ErrNativePrivateBinding
		}
		if err := validate(ctx, binding); err != nil {
			return time.Time{}, nil, err
		}
		lease, err := admission.Admit(ctx, time.Time{}, nil)
		if err != nil {
			return time.Time{}, nil, err
		}
		lease.Poll(func(checkCtx context.Context) error {
			err := validate(checkCtx, binding)
			if errors.Is(err, server.ErrNativePrivateBinding) || errors.Is(err, admission.ErrRevoked) {
				return admission.ErrRevoked
			}
			return err
		})
		revoked := lease.Context().Done()
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
	if config.controlURL == "" || config.issuer == "" || config.stateRoot == "" || config.machineID == "" || config.generation == 0 || config.transport == nil || config.identity == nil || config.keys == nil || (config.dispatch == nil && (config.authorizer == nil || config.serve == nil || config.transfer == nil)) {
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
			s.recordPeerState(runCtx, nil, err)
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
	s.recordPeerState(runCtx, generation, nil)
	go s.supervise(runCtx, generation)
	return nil
}

// Approval and transient control failures keep the existing supervisor alive.
// Invalid identity, protocol, or authorization still fails startup closed.
// A peer network the server refuses to configure (the account's configuration is too large, or the
// key generation moved during registration) leaves only peer networking down: the carrier keeps
// serving exec, terminals and previews through the edge and retries peer networking. Exiting instead
// stopped every fresh pair on a large account (2026-10-22).
func transientNativePeerStart(err error) bool {
	if errors.Is(err, errNativePeerPending) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var response *clientapi.APIError
	if errors.As(err, &response) {
		return response.Status == 408 || response.Status == 429 || response.Status >= 500 ||
			response.Status == 409 && (response.Code == "peer_network_too_large" || response.Code == "peer_network_conflict")
	}
	var refused *peeridentityenrollment.StatusError
	if errors.As(err, &refused) {
		return refused.Transient()
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
	err = authority.RegisterOrRestore(request, control, false)
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
	owner, err := native.NewOwner(native.Config{Authority: authority, TLS: tlsConfig, AdmitOffline: offlineAdmission(s.config.offlineAudit, s.config.dispatch != nil, s.config.networkEvents.Healthy), MeterIncoming: func(conn net.Conn, binding native.MeterBinding) net.Conn {
		return usage.Wrap(conn, bandwidth.Binding{AccessSessionID: binding.AccessSessionID, StreamID: binding.StreamID, Consumer: binding.Consumer, Reverse: binding.Reverse}, func(direction string) bandwidth.Path {
			mode, node := binding.Path(direction)
			return bandwidth.Path{Mode: mode, NodeID: node}
		})
	}, Observe: func(event native.Event) {
		if event.Err == nil {
			return
		}
		switch event.Kind {
		case "accept_failed":
			// These workers have no foreground caller to own their final failure.
			if errorreport.ProjectFault(ctx, "paperboat-daemon", "peer_stream", "peer_connect", "native_private_failed", event.Err).Outcome == "canceled" {
				return
			}
			errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "peer_stream", "peer_connect", "native_private_failed", event.Err)
		case "serve_failed":
			if errorreport.ProjectFault(ctx, "paperboat-daemon", "peer_stream", "lifecycle", "service_failed", event.Err).Outcome == "canceled" {
				return
			}
			errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "peer_stream", "lifecycle", "service_failed", event.Err)
		case "dial_failed":
			if errorreport.ProjectFault(ctx, "paperboat-daemon", "peer_stream", "peer_connect", "native_private_failed", event.Err).Outcome == "canceled" {
				return
			}
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
	if s.config.dispatch != nil {
		appsConfig.Authorize = s.config.dispatch.Authorize
		appsConfig.ServeAuthorizedTransfer = s.serveStream
		appsConfig.ServeTransfer = nil
		appsConfig.ServeHTTP3 = func(serveCtx context.Context, session *native.Session, authorize func(context.Context, streamauth.Header) (string, error)) error {
			return server.ServeNativePrivateHTTP3WithBindingDial(serveCtx, session, authorize, s.config.dispatch.Current, s.config.dispatch.OpenPrivate, nil)
		}
	} else if s.config.privateCurrent != nil && s.config.privateDial != nil {
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
	generation := &productionNativePeerGeneration{cancel: cancel, owner: owner, apps: apps, errors: make(chan error, 2), identityFingerprint: certificate.Fingerprint(), authority: authority, requestRefresh: authority.RequestRefresh, control: control}
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
		timer := time.NewTimer(productionNativePeerRestartDelay)
		defer timer.Stop()
		failures := 0
		for generation == nil {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			next, err := s.startGeneration(ctx)
			s.recordPeerState(ctx, next, err)
			if err == nil {
				generation = next
				continue
			}
			failures++
			timer.Reset(nativePeerRestartDelay(failures))
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
			s.recordPeerState(ctx, nil, err)
			stopErr := generation.stop()
			if stopErr != nil {
				errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "peer_identity", "component_shutdown", "service_failed", stopErr)
			}
			s.mu.Lock()
			s.lastErr = errors.Join(s.lastErr, stopErr)
			s.mu.Unlock()
		case <-s.config.networkEvents.Changed():
			if generation.requestRefresh != nil {
				generation.requestRefresh()
			}
		case <-identityCheck.C:
			fingerprint, err := s.currentIdentityFingerprint()
			if err == nil && fingerprint == generation.identityFingerprint {
				continue
			}
			if err == nil {
				err = errors.New("native endpoint identity changed")
				s.recordPeerEvent(ctx, "identity_renewed")
			} else {
				s.recordPeerState(ctx, nil, err)
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
		for failures := 1; ; failures++ {
			next, err := s.startGeneration(ctx)
			s.recordPeerState(ctx, next, err)
			if err == nil {
				generation = next
				break
			}
			timer.Reset(nativePeerRestartDelay(failures))
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
// Publish the active generation before any reader or observer can see recovery.
func (s *productionNativePeerService) recordPeerState(
	ctx context.Context, generation *productionNativePeerGeneration, err error,
) {
	s.mu.Lock()
	previous := s.lastErr
	if err == nil {
		s.current = generation
	}
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

// PathEligibility reports which kind of path carries the peer endpointID's traffic to this host and a
// channel closed once it changes or ctx ends (tailnet.Authority.PathEligibility). A receiver refuses an
// Unlimited file transfer while it is a hosted relay (PLAN 3b "File transfer modes").
func (s *productionNativePeerService) PathEligibility(ctx context.Context, endpointID string) (tailnet.PathKind, <-chan struct{}) {
	s.mu.Lock()
	generation := s.current
	s.mu.Unlock()
	if generation == nil || generation.authority == nil {
		return tailnet.PathNone, closedWhenDone(ctx)
	}
	return generation.authority.PathEligibility(ctx, endpointID)
}

// closedWhenDone is a change channel for a peer engine that is not running: it closes with ctx.
func closedWhenDone(ctx context.Context) <-chan struct{} {
	changed := make(chan struct{})
	go func() { <-ctx.Done(); close(changed) }()
	return changed
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
	if s.config.dispatch != nil && header.Consumer != "ssh" {
		remote, err := s.config.dispatch.Open(ctx, header)
		if err != nil {
			return err
		}
		defer remote.Close()
		stop := context.AfterFunc(ctx, func() { _ = remote.Close(); _ = stream.Close() })
		defer stop()
		return server.BridgeNativeDispatch(stream, remote)
	}

	switch header.Consumer {
	case "inspector":
		return s.serveInspector(ctx, header, stream)
	case "terminal", "exec", "config_compare":
		return s.config.serve(stream)
	case "ssh":
		if s.config.dispatch != nil {
			target, err := s.config.dispatch.SSHTarget(ctx)
			if err != nil {
				return err
			}
			if s.config.ssh == nil {
				return ErrProductionInvalid
			}
			if _, err = s.config.ssh.ReconcileSourceTarget(ctx, target.Generation, target.Port, target.SourceSHA256); err != nil {
				return err
			}
		}
		if s.config.ssh == nil {
			return ErrProductionInvalid
		}
		if peer, ok := native.PeerFromContext(ctx); ok {
			// Every SSH stream is admitted from the host's signed rules (D1); it is offline only while the host
			// hears nothing from Paperboat.
			offline := header.Offline && !s.config.networkEvents.Healthy()
			ctx = managedssh.WithPeer(ctx, managedssh.Peer{AccountID: peer.AccountID, EndpointID: peer.EndpointID, OperationID: header.OperationID, Offline: offline})
		}
		target, ok := s.config.ssh.Target()
		if !ok {
			s.config.ssh.Refused(ctx, stream, 0, managedssh.RefusedNotSetUp)
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
