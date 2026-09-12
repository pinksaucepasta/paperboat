//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"sync"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/peerrelay"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
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

type productionNativePrivateValidator interface {
	ValidateNativePrivateTarget(nativeprivate.Binding) error
}

func productionNativeCurrent(validators ...productionNativePrivateValidator) server.NativePrivateTCPCurrent {
	if len(validators) == 0 {
		return nil
	}
	validate := func(binding nativeprivate.Binding) error {
		for _, validator := range validators {
			if validator != nil && validator.ValidateNativePrivateTarget(binding) == nil {
				return nil
			}
		}
		return server.ErrNativePrivateBinding
	}
	return func(ctx context.Context, binding nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
		if ctx == nil || validate(binding) != nil {
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
					if validate(binding) != nil {
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

func (s *productionNativePeerService) buildGeneration(ctx context.Context) (*productionNativePeerGeneration, error) {
	endpoint, err := runtimeEnvironmentEndpoint(s.config.stateRoot)
	if err != nil || endpoint.Generation != s.config.generation {
		return nil, errors.Join(ErrProductionInvalid, err)
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
	if err := authority.ConfigureDeviceRelay(deviceRelayAddresses(41642)); err != nil {
		_ = authority.Close()
		return nil, errors.Join(ErrProductionInvalid, errors.New("device peer relay requires a usable unicast network address on UDP port 41642"), err)
	}
	regions, err := authority.ConfigureRegionalRelays(relayTLS)
	if err != nil {
		_ = authority.Close()
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	owner, err := native.NewOwner(native.Config{Authority: authority, TLS: tlsConfig, RefreshAuthority: func(refreshCtx context.Context) error {
		request, cancel := context.WithTimeout(refreshCtx, 15*time.Second)
		defer cancel()
		return authority.Refresh(request, control)
	}})
	if err != nil {
		_ = authority.Close()
		return nil, err
	}
	appsConfig := nativesession.Config{Authorize: s.inspectorNetworkAuthorizer(), ServeTransfer: func(serveCtx context.Context, connection net.Conn) error {
		return server.ServeHTTPConnection(serveCtx, connection, s.config.transfer)
	}, ServeStream: s.serveStream}
	if s.config.privateCurrent != nil && s.config.privateDial != nil {
		appsConfig.ServeHTTP3 = func(serveCtx context.Context, session *native.Session, authorize func(context.Context, streamauth.Header) (string, error)) error {
			return server.ServeNativePrivateHTTP3(serveCtx, session, authorize, s.config.privateCurrent, s.config.privateDial, s.config.inspectorStore)
		}
	}
	apps, err := nativesession.New(appsConfig)
	if err != nil {
		_ = owner.Close()
		return nil, err
	}
	var firstRegion *tailcfg.DERPRegion
	if len(regions) != 0 {
		firstRegion = regions[0]
	}
	if _, err := authority.Listen(firstRegion); err != nil {
		_ = owner.Close()
		return nil, err
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
		generation.errors <- owner.Listen(runCtx, firstRegion, apps.Serve)
	}()
	return generation, nil
}

func (s *productionNativePeerService) ReconcilePeerRelay(ctx context.Context, _ bool) error {
	s.mu.Lock()
	generation := s.current
	s.mu.Unlock()
	if generation == nil || generation.authority == nil || generation.control == nil {
		return ErrProductionInvalid
	}
	request, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return generation.authority.Refresh(request, generation.control)
}

func deviceRelayAddresses(port uint16) []netip.AddrPort {
	addresses, _ := net.InterfaceAddrs()
	result := make([]netip.AddrPort, 0, 4)
	for _, value := range addresses {
		prefix, err := netip.ParsePrefix(value.String())
		if err != nil || !prefix.Addr().IsValid() || prefix.Addr().IsLoopback() || prefix.Addr().IsUnspecified() || prefix.Addr().IsMulticast() {
			continue
		}
		result = append(result, netip.AddrPortFrom(prefix.Addr().Unmap(), port))
		if len(result) == 4 {
			break
		}
	}
	return result
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
		cleanupErr := generation.stop()
		s.mu.Lock()
		s.current = nil
		s.cancel = nil
		s.cleanupErr = errors.Join(s.cleanupErr, cleanupErr)
		close(s.done)
		s.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-generation.errors:
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			s.lastErr = err
			s.mu.Unlock()
			stopErr := generation.stop()
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
			}
			s.mu.Lock()
			s.lastErr = err
			s.mu.Unlock()
			stopErr := generation.stop()
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
			if err == nil {
				generation = next
				s.mu.Lock()
				s.current = next
				s.lastErr = nil
				s.mu.Unlock()
				break
			}
			s.mu.Lock()
			s.lastErr = err
			s.mu.Unlock()
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
	case "terminal", "exec":
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
	authorize := peerrelay.CredentialStreamAuthorizer(factory)
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
