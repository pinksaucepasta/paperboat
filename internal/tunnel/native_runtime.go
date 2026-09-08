package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	hostauth "github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

type nativeRuntimeClock struct{}

func (nativeRuntimeClock) Now() time.Time { return time.Now().UTC() }

type cliNativeNetworkAPI struct {
	issuer string
	auth   config.AuthSource
	http   *http.Client
}

func (c cliNativeNetworkAPI) client() (*api.Client, error) {
	credential, err := c.auth.Credential()
	if err != nil {
		return nil, err
	}
	return api.New(c.issuer, credential, c.http), nil
}

func (c cliNativeNetworkAPI) RegisterPeerNetwork(ctx context.Context, request api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	client, err := c.client()
	if err != nil {
		return api.PeerNetworkRegistrationResult{}, err
	}
	return client.RegisterPeerNetwork(ctx, request)
}

func (c cliNativeNetworkAPI) PeerNetworkConfiguration(ctx context.Context, operationID string) (api.PeerNetworkConfigurationResult, error) {
	client, err := c.client()
	if err != nil {
		return api.PeerNetworkConfigurationResult{}, err
	}
	return client.PeerNetworkConfiguration(ctx, operationID)
}

// cliNativeRuntime owns the one CLI-role authority and Tailcat engine shared
// by every admitted machine. Application sessions are still independently
// authorized and may be closed without replacing this runtime.
type cliNativeRuntime struct {
	accountID, endpointID string
	identityFingerprint   string
	authority             *tailnet.Authority
	owner                 *native.Owner
	identity              clientauthority.Authority
	cancel                context.CancelFunc
	done                  chan struct{}
	once                  sync.Once
}

type cliNativeStreamGroup struct {
	session     nativeApplicationSession
	target      *resolver.TerminalTarget
	application peerApplication
	consumer    string
	now         func() time.Time
	sequence    atomic.Uint64
}

type cliNativeTransferLease struct {
	runtime     *cliNativeRuntime
	dial        func(context.Context) (nativeApplicationSession, error)
	machineID   string
	target      *resolver.TerminalTarget
	application peerApplication
	now         func() time.Time
	mu          sync.Mutex
	session     nativeApplicationSession
	closed      bool
}

func (l *cliNativeTransferLease) OpenTransferStream(ctx context.Context) (net.Conn, error) {
	if l == nil || ctx == nil {
		return nil, ErrPeerTerminalInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, net.ErrClosed
	}
	for attempt := 0; attempt < 2; attempt++ {
		if l.session == nil {
			var session nativeApplicationSession
			var err error
			if l.dial != nil {
				session, err = l.dial(ctx)
			} else if l.runtime != nil {
				session, err = l.runtime.Dial(ctx, l.machineID, peerquic.ClassTransfer)
			} else {
				err = ErrPeerTerminalInvalid
			}
			if err != nil {
				return nil, err
			}
			l.session = session
		}
		group := &cliNativeStreamGroup{session: l.session, target: l.target, application: l.application, consumer: "file_transfer", now: l.now}
		connection, err := group.OpenTransferStream(ctx)
		if err == nil {
			return connection, nil
		}
		_ = l.session.Close()
		l.session = nil
	}
	return nil, net.ErrClosed
}

func (l *cliNativeTransferLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.session != nil {
		err := l.session.Close()
		l.session = nil
		return err
	}
	return nil
}

type nativeApplicationSession interface {
	OpenAuthorized(context.Context, streamauth.Header, string, string) (net.Conn, error)
	Close() error
}

func (g *cliNativeStreamGroup) OpenStream(ctx context.Context) (nativeStream, error) {
	streamID := "native_" + strconv.FormatUint(g.sequence.Add(1), 10)
	header, err := g.application.authorizationHeader(g.target, g.consumer, g.application.operationID, streamID, g.now().UTC())
	if err != nil {
		return nil, err
	}
	return g.session.OpenAuthorized(ctx, header, g.target.Auth.ResourceID, nativeCapability(g.consumer))
}

func (g *cliNativeStreamGroup) Close() error { return g.session.Close() }

func (g *cliNativeStreamGroup) OpenTransferStream(ctx context.Context) (net.Conn, error) {
	stream, err := g.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	connection, ok := stream.(net.Conn)
	if !ok {
		_ = stream.Close()
		return nil, ErrPeerTerminalInvalid
	}
	return connection, nil
}

type cliNativeAuthorizedStream struct {
	net.Conn
	session nativeApplicationSession
	once    sync.Once
	err     error
}

func (s *cliNativeAuthorizedStream) Close() error {
	s.once.Do(func() { s.err = errors.Join(s.Conn.Close(), s.session.Close()) })
	return s.err
}

func nativeCapability(consumer string) string {
	switch consumer {
	case "terminal", "exec", "ssh":
		return "terminal"
	case "codex":
		return "codex"
	case "file_transfer":
		return "file_transfer"
	case "private_tcp", "private_http":
		return "private_access"
	default:
		return ""
	}
}

func (r *cliNativeRuntime) openApplication(ctx context.Context, machineID, consumer string, application peerApplication, target *resolver.TerminalTarget, now func() time.Time) (Conn, error) {
	if target == nil || target.Auth.ResourceID == "" || nativeCapability(consumer) == "" {
		return nil, ErrPeerTerminalInvalid
	}
	session, err := r.Dial(ctx, machineID, peerquic.ClassInteractive)
	if err != nil {
		return nil, err
	}
	group := &cliNativeStreamGroup{session: session, target: target, application: application, consumer: consumer, now: now}
	if application.helper != nil {
		message, err := authenticateNativeStreamGroup(ctx, group, target, "native")
		if err != nil {
			_ = session.Close()
			return nil, err
		}
		connection, err := application.helper(ctx, message, target)
		if err != nil {
			_ = message.Close()
		}
		return connection, err
	}
	if application.raw != nil {
		stream, err := group.OpenStream(ctx)
		if err != nil {
			_ = session.Close()
			return nil, err
		}
		networkStream, ok := stream.(net.Conn)
		if !ok {
			_ = stream.Close()
			_ = session.Close()
			return nil, ErrPeerTerminalInvalid
		}
		connection, err := application.raw(ctx, &cliNativeAuthorizedStream{Conn: networkStream, session: session})
		if err != nil {
			_ = stream.Close()
			_ = session.Close()
		}
		return connection, err
	}
	_ = session.Close()
	return nil, ErrPeerTerminalInvalid
}

// PrepareNativeFileTransfer retains one authorized native association in the
// daemon and exposes only application-stream opening to the local CLI.
func (t *PeerTerminalTunnel) PrepareNativeFileTransfer(ctx context.Context, info resolver.ConnectInfo, operationID string) (DirectTransferStreamOpener, error) {
	if t == nil || ctx == nil || info.TargetKind != "machine" || info.ProjectID == "" || info.MachineGeneration == 0 || info.Terminal == nil || info.Terminal.EnvironmentID == "" || info.Terminal.Auth.ResourceID == "" || operationID == "" {
		return nil, ErrPeerTerminalInvalid
	}
	credential, err := t.config.Auth.Credential()
	if err != nil {
		return nil, err
	}
	profile, err := t.config.Store.Load(t.config.Issuer)
	if err != nil {
		return nil, err
	}
	client := api.New(t.config.Issuer, credential, t.config.HTTPClient)
	authority, err := t.authorities.Resolve(ctx, clientauthority.Request{Store: t.config.Store, Client: client, Issuer: t.config.Issuer, AccountID: profile.Account.ID, CLIClientSessionID: profile.CLIClientSessionID, MachineID: info.ProjectID, MachineGeneration: info.MachineGeneration, Now: t.config.Now().UTC()})
	if err != nil {
		return nil, err
	}
	runtime, consumed, err := t.acquireNativeRuntime(ctx, profile.Account.ID, profile.CLIClientSessionID, authority)
	if !consumed {
		authority.Clear()
	}
	if err != nil {
		return nil, err
	}
	return &cliNativeTransferLease{runtime: runtime, machineID: info.ProjectID, target: info.Terminal, application: peerApplication{operationID: operationID}, now: t.config.Now}, nil
}

func (t *PeerTerminalTunnel) acquireNativeRuntime(ctx context.Context, accountID, endpointID string, identity clientauthority.Authority) (*cliNativeRuntime, bool, error) {
	if t == nil {
		return nil, false, ErrPeerTerminalInvalid
	}
	t.nativeMu.Lock()
	defer t.nativeMu.Unlock()
	if t.nativeClosed {
		return nil, false, net.ErrClosed
	}
	fingerprint := identity.LocalCertificate.Fingerprint()
	networkAPI := cliNativeNetworkAPI{issuer: t.config.Issuer, auth: t.config.Auth, http: t.config.HTTPClient}
	if current := t.nativeRuntime; current != nil && current.alive() && current.accountID == accountID && current.endpointID == endpointID && current.identityFingerprint == fingerprint && identity.LocalCertificate.Claims.ExpiresAt.After(time.Now().UTC()) {
		// The operation descriptor may have just created its peer grant. Refresh
		// signed admission before reusing the engine; the periodic refresh alone
		// cannot establish that this new operation is ready to open a stream.
		refresh, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := current.authority.Refresh(refresh, networkAPI)
		cancel()
		if err != nil {
			return nil, false, err
		}
		return current, false, nil
	}
	if t.nativeRuntime != nil {
		_ = t.nativeRuntime.Close()
		t.nativeRuntime = nil
	}
	runtime, err := newCLINativeRuntime(ctx, t.config.Issuer, t.config.Store, networkAPI, t.config.HTTPClient, t.config.TLS, accountID, endpointID, identity)
	if err != nil {
		return nil, false, err
	}
	runtime.identityFingerprint = fingerprint
	t.nativeRuntime = runtime
	return runtime, true, nil
}

func (r *cliNativeRuntime) alive() bool {
	if r == nil || r.done == nil {
		return false
	}
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

func newCLINativeRuntime(ctx context.Context, issuer string, store config.ProfileStore, networkAPI tailnet.NetworkAPI, httpClient *http.Client, relayTrust *tls.Config, accountID, endpointID string, identity clientauthority.Authority) (*cliNativeRuntime, error) {
	if ctx == nil || issuer == "" || store.Path == "" || store.Secrets == nil || networkAPI == nil || httpClient == nil || relayTrust == nil || accountID == "" || endpointID == "" {
		return nil, ErrPeerTerminalInvalid
	}
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, ErrPeerTerminalInvalid
	}
	fetcher, err := hostauth.NewHTTPJWKSFetcher(parsed.ResolveReference(&url.URL{Path: "/.well-known/jwks.json"}).String(), []string{parsed.Hostname()}, httpClient.Transport)
	if err != nil {
		return nil, err
	}
	keys, err := hostauth.NewJWKSCache(hostauth.JWKSConfig{Fetcher: fetcher, Clock: nativeRuntimeClock{}, TTL: 5 * time.Minute, RetainMissing: hostauth.DefaultRetainMissing})
	if err != nil {
		return nil, err
	}
	refresh, cancelRefresh := context.WithTimeout(ctx, 10*time.Second)
	err = keys.Refresh(refresh)
	cancelRefresh()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	lifetime := identity.LocalCertificate.Claims.ExpiresAt.Sub(now)
	if lifetime > 24*time.Hour {
		lifetime = 24 * time.Hour
	}
	issueLeaf := func() (tls.Certificate, error) {
		remaining := identity.LocalCertificate.Claims.ExpiresAt.Sub(time.Now().UTC())
		if remaining > 24*time.Hour {
			remaining = 24 * time.Hour
		}
		return endpointidentity.NewTLSCertificate(identity.LocalCertificate, identity.RootPublic, identity.LocalKeys.QUICPrivate, time.Now().UTC(), remaining)
	}
	leaf, err := endpointidentity.NewTLSCertificate(identity.LocalCertificate, identity.RootPublic, identity.LocalKeys.QUICPrivate, now, lifetime)
	if err != nil {
		return nil, err
	}
	getCertificate := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		value, issueErr := issueLeaf()
		return &value, issueErr
	}
	getClientCertificate := func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		value, issueErr := issueLeaf()
		return &value, issueErr
	}
	peerTLS := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{leaf}, GetCertificate: getCertificate, GetClientCertificate: getClientCertificate} //nolint:gosec
	relayTLS := relayTrust.Clone()
	relayTLS.MinVersion = tls.VersionTLS13
	relayTLS.Certificates = []tls.Certificate{leaf}
	relayTLS.GetClientCertificate = getClientCertificate
	self := tailnet.NetworkBinding{AccountID: accountID, EndpointID: endpointID, Role: "cli", EndpointGeneration: identity.LocalCertificate.Claims.Generation, QUICCertificateFingerprint: identity.LocalCertificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.LocalCertificate.Claims.QUICPublicKey)}
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: store, Issuer: issuer, Self: self, Keys: keys})
	if err != nil {
		return nil, err
	}
	register, cancelRegister := context.WithTimeout(ctx, 15*time.Second)
	err = authority.Register(register, networkAPI, false)
	cancelRegister()
	if err != nil {
		_ = authority.Close()
		return nil, err
	}
	if _, err = authority.ConfigureRegionalRelays(relayTLS); err != nil {
		_ = authority.Close()
		return nil, err
	}
	owner, err := native.NewOwner(native.Config{Authority: authority, TLS: peerTLS})
	if err != nil {
		_ = authority.Close()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	runtime := &cliNativeRuntime{accountID: accountID, endpointID: endpointID, authority: authority, owner: owner, identity: identity, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(runtime.done)
		_ = authority.Run(runCtx, networkAPI, nil)
	}()
	return runtime, nil
}

func (r *cliNativeRuntime) Dial(ctx context.Context, machineID string, class peerquic.Class) (*native.Session, error) {
	if r == nil || r.owner == nil || machineID == "" {
		return nil, ErrPeerTerminalInvalid
	}
	descriptor, err := r.authority.Descriptor(machineID)
	if err != nil {
		return nil, err
	}
	return r.owner.Dial(ctx, descriptor, machineID, class)
}

func (r *cliNativeRuntime) Close() error {
	if r == nil {
		return nil
	}
	var result error
	r.once.Do(func() {
		r.cancel()
		result = r.owner.Close()
		<-r.done
		r.identity.Clear()
	})
	return errors.Join(result)
}
