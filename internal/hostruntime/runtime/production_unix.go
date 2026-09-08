//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/availability"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/codexsession"
	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hosted"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/observability"
	peeridentityenrollment "github.com/pinksaucepasta/paperboat/internal/hostruntime/peeridentity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/peerrelay"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/networkcheck"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/relayselection"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/signaling"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/transfercrypto"
)

var (
	ErrProductionInvalid          = errors.New("invalid production host configuration")
	ErrManagedSSHUnavailable      = errors.New("managed SSH host authority is unavailable")
	errEnvironmentEndpointPending = errors.New("environment endpoint enrollment is pending")
)

type productionClock struct{}

func (productionClock) Now() time.Time { return time.Now().UTC() }

type productionClientPeerDependencies struct {
	attempts            peerrelay.Source
	networkChanges      *networkChangeService
	signalingSubstrate  *signaling.SubstrateManager
	stateRoot           string
	transport           http.RoundTripper
	authorizer          server.AuthorizerFactory
	transferKeys        *transfercrypto.KeyVault
	observeRelaySuccess func(string)
	directNetwork       *directNetworkProxy
	build               func(peerrelay.Config) (*peerrelay.Service, error)
}

func newProductionClientPeerService(dependencies productionClientPeerDependencies, serve func(net.Conn) error, transferHandler http.Handler) (Service, error) {
	service, err := dependencies.build(peerrelay.Config{
		Source:             dependencies.attempts,
		Fingerprints:       dependencies.networkChanges,
		SocketMapping:      dependencies.networkChanges,
		SignalingSubstrate: dependencies.signalingSubstrate,
		StateRoot:          dependencies.stateRoot,
		TLS:                &tls.Config{MinVersion: tls.VersionTLS13},
		HTTPClient:         &http.Client{Transport: dependencies.transport},
		Serve:              serve,
		ServeTransfer: func(ctx context.Context, stream net.Conn) error {
			return server.ServeHTTPConnection(ctx, stream, transferHandler)
		},
		AuthorizeStream:     peerrelay.CredentialStreamAuthorizer(dependencies.authorizer),
		ServeStream:         func(context.Context, streamauth.Header, net.Conn) error { return peerrelay.ErrInvalid },
		TransferKeys:        dependencies.transferKeys,
		ObserveRelaySuccess: dependencies.observeRelaySuccess,
		ObserveTransferKeyAcknowledged: func() {
			recordProductionPeerOutcome(dependencies.stateRoot, "transfer_key_ack_written")
		},
		ObserveError: func(err error) {
			observeProductionPeerError(dependencies.stateRoot, err)
		},
	})
	if err == nil {
		dependencies.directNetwork.Set(service)
	}
	return service, err
}

type regionalMonitorService struct {
	monitor *networkcheck.RegionalMonitor
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
}

type currentRelayRegion struct {
	mu         sync.RWMutex
	region     string
	observedAt time.Time
}

func (s *currentRelayRegion) Current() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.region
}

func (s *currentRelayRegion) Observe(region string) {
	if region == "" {
		return
	}
	s.mu.Lock()
	s.region = region
	s.observedAt = time.Now().UTC()
	s.mu.Unlock()
}

func (s *currentRelayRegion) Success() (string, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.region, s.observedAt
}

func (s *regionalMonitorService) NetworkChanged() { s.monitor.NetworkChanged() }

func (s *regionalMonitorService) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.cancel, s.done = cancel, done
	go func() {
		defer close(done)
		_ = s.monitor.Run(runCtx)
	}()
	return nil
}

func (s *regionalMonitorService) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newProductionRegionalMonitor(controlURL *url.URL, transport http.RoundTripper, current func() string, substrate *signaling.SubstrateManager, signalingTLS *tls.Config, warmMapping func(context.Context, uint64, string) error) (*regionalMonitorService, *networkcheck.RegionalCache, error) {
	if controlURL == nil || transport == nil || current == nil {
		return nil, nil, ErrProductionInvalid
	}
	client := clientapi.New(controlURL.String(), clientconfig.Credential{}, &http.Client{Transport: transport, Timeout: 10 * time.Second})
	cache := networkcheck.NewRegionalCache()
	probe, err := networkcheck.NewRegionalProbe(networkcheck.RegionalProbeConfig{
		Timeout: 3 * time.Second,
		STUN:    networkcheck.STUNRegionalLatency(net.DefaultResolver, 1500*time.Millisecond),
		HTTPS:   networkcheck.HTTPSRegionalLatency(time.Now, &http.Client{Transport: transport, Timeout: 3 * time.Second}),
	})
	if err != nil {
		return nil, nil, err
	}
	monitor, err := networkcheck.NewRegionalMonitor(networkcheck.RegionalMonitorConfig{
		Inventory: func(ctx context.Context) ([]networkcheck.ProbeRegion, error) {
			document, inventoryErr := client.NetworkCheckRegions(ctx)
			if inventoryErr != nil {
				return nil, inventoryErr
			}
			regions := make([]networkcheck.ProbeRegion, 0, len(document.Regions))
			for _, region := range document.Regions {
				regions = append(regions, networkcheck.ProbeRegion{Region: region.Region, STUNURL: region.STUNURL, HTTPSURL: region.HTTPSURL})
			}
			if substrate != nil && len(regions) > 0 {
				if warmMapping != nil {
					warmCtx, cancel := context.WithTimeout(ctx, time.Second)
					_ = warmMapping(warmCtx, 1, regions[0].STUNURL)
					cancel()
				}
				var signalingWarm sync.WaitGroup
				var signalingWarmMu sync.Mutex
				var signalingWarmErr error
				for _, region := range regions[:min(len(regions), 16)] {
					signalingURL, urlErr := signalingURLFromRegionalProbe(region.HTTPSURL)
					if urlErr != nil {
						continue
					}
					signalingWarm.Add(1)
					go func() {
						defer signalingWarm.Done()
						warmCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
						defer cancel()
						if warmErr := substrate.Warm(warmCtx, signalingURL, signalingTLS); warmErr != nil {
							signalingWarmMu.Lock()
							signalingWarmErr = errors.Join(signalingWarmErr, warmErr)
							signalingWarmMu.Unlock()
						}
					}()
				}
				signalingWarm.Wait()
				if signalingWarmErr != nil {
					return nil, signalingWarmErr
				}
			}
			return regions, nil
		},
		Probe: probe, Cache: cache, Clock: time.Now, CurrentRegion: current,
		FullInterval: 5 * time.Minute, IncrementalInterval: time.Minute,
		Jitter: func(value time.Duration) time.Duration {
			return time.Duration(float64(value) * (0.9 + mathrand.Float64()*0.2))
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return &regionalMonitorService{monitor: monitor}, cache, nil
}

func signalingURLFromRegionalProbe(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", ErrProductionInvalid
	}
	parsed.Scheme = "wss"
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "/v1/peer-signaling", "", "", ""
	return parsed.String(), nil
}

func NewProductionHost(ctx context.Context, version string, environ func(string) string) (*Host, error) {
	return newProductionHost(ctx, version, environ, nil)
}

// NewProductionHostWithTunnelAssembly enables the connector-v1 stable
// tunnel-manager composition. The provider is deliberately explicit because
// current deployment configuration does not contain the server-issued
// connector identity, renewable credential reference, carrier certificates,
// or route authorizer needed to construct it safely.
func NewProductionHostWithTunnelAssembly(ctx context.Context, version string, environ func(string) string, provider ProductionTunnelAssemblyProvider) (*Host, error) {
	if provider == nil {
		return nil, errors.Join(ErrProductionInvalid, ErrProductionTunnelAssemblyRequired)
	}
	return newProductionHost(ctx, version, environ, provider)
}

func newProductionHost(ctx context.Context, version string, environ func(string) string, tunnelProvider ProductionTunnelAssemblyProvider) (*Host, error) {
	if environ == nil {
		return nil, ErrProductionInvalid
	}
	runtimeConfig, err := runtimeconfig.FromEnv(version, environ)
	if err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	bootState, recoveryExitSignal, err := recordWorkerBoot(runtimeConfig.StateRoot)
	if err != nil {
		return nil, err
	}
	metrics, err := observability.NewRegistry(observability.DefaultDescriptors())
	if err != nil {
		return nil, err
	}
	_ = metrics.Record("paperboat_runtime_restart_total", float64(bootState.Generation), nil)
	if runtimeConfig.Profile == runtimeconfig.BYOD {
		store, openErr := runtimeidentity.Open(runtimeidentity.Config{StateRoot: runtimeConfig.StateRoot})
		if openErr == nil {
			registration, registrationErr := store.Registration()
			if registrationErr == nil && shouldRunClientCoordinator(registration.SetupMode, environ("PAPERBOAT_SETUP_MODE")) {
				return newProductionClientCoordinator(ctx, version, environ, runtimeConfig, bootState, recoveryExitSignal, metrics, registration, tunnelProvider)
			}
		}
	}
	lazyBootBytes := make([]byte, 24)
	if _, err := rand.Read(lazyBootBytes); err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	lazyBootID := hex.EncodeToString(lazyBootBytes)
	lazyStartedAt := time.Now().UTC()
	var hostedConfig hosted.Config
	if runtimeConfig.Profile == runtimeconfig.Hosted {
		hostedConfig, err = hosted.FromEnv(environ)
		if err != nil {
			return nil, err
		}
		if setupName := environ("PAPERBOAT_SETUP_SCRIPT_ENV"); setupName != "" && safeProductionEnvironmentName(setupName) {
			_ = os.Unsetenv(setupName)
		}
	}
	controlURL, err := validatedControlURL(environ("PAPERBOAT_CONTROL_URL"))
	if err != nil {
		return nil, err
	}
	issuer := strings.TrimRight(valueOrRuntime(environ("PAPERBOAT_CONTROL_ISSUER"), controlURL.String()), "/")
	transport, err := productionTransport(environ("PAPERBOAT_CONTROL_CA_FILE"), environ)
	if err != nil {
		return nil, err
	}
	operationID := func() (string, error) {
		bytes := make([]byte, 16)
		if _, err := rand.Read(bytes); err != nil {
			return "", err
		}
		return "op_admit_" + hex.EncodeToString(bytes), nil
	}
	renewingTokens, err := enrollment.NewRenewingTokenSource(enrollment.RenewingTokenConfig{ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, Transport: transport, RenewBefore: 10 * time.Minute, Timeout: 15 * time.Second, Clock: func() time.Time { return time.Now().UTC() }, OperationID: operationID, Metrics: metrics})
	if err != nil {
		return nil, err
	}
	var enrollmentClient *enrollment.Client
	if _, loadErr := enrollment.LoadRuntimeIdentityForRenewal(runtimeConfig.StateRoot, time.Now().UTC()); loadErr != nil {
		var clientErr error
		enrollmentClient, clientErr = enrollment.NewClient(transport, 15*time.Second)
		if clientErr != nil {
			return nil, clientErr
		}
		enrollmentConfig := enrollment.Config{
			ControlURL: controlURL.String(), ControlCAFile: environ("PAPERBOAT_CONTROL_CA_FILE"),
			StateRoot: runtimeConfig.StateRoot,
		}
		if runtimeConfig.Profile == runtimeconfig.Hosted {
			_, err = retryHostedControl(ctx, func(attemptCtx context.Context) (enrollment.RuntimeIdentity, error) {
				return enrollmentClient.EnrollHosted(attemptCtx, enrollmentConfig)
			})
		} else {
			grantName := valueOrRuntime(environ("PAPERBOAT_ENROLLMENT_CREDENTIAL_ENV"), "PAPERBOAT_ENROLLMENT_CREDENTIAL")
			if !safeProductionEnvironmentName(grantName) {
				return nil, ErrProductionInvalid
			}
			enrollmentConfig.EnrollmentCredential = environ(grantName)
			if enrollmentConfig.EnrollmentCredential == "" {
				return nil, loadErr
			}
			_, err = enrollmentClient.Enroll(ctx, enrollmentConfig)
			_ = os.Unsetenv(grantName)
		}
		if err != nil {
			if runtimeConfig.Profile == runtimeconfig.Hosted {
				return nil, fmt.Errorf("hosted identity bootstrap: %w", err)
			}
			return nil, err
		}
	}
	identity, err := enrollment.LoadRuntimeIdentityForRenewal(runtimeConfig.StateRoot, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	machineID := environ("PAPERBOAT_MACHINE_ID")
	if machineID == "" || machineID != identity.MachineID {
		return nil, ErrProductionInvalid
	}
	inboxPath := ""
	var managedSSHIdentity managedSSHIdentitySource
	var networkFingerprintSecret []byte
	var machineRegistration runtimeidentity.Registration
	if runtimeConfig.Profile == runtimeconfig.BYOD {
		identityStore, openErr := runtimeidentity.Open(runtimeidentity.Config{StateRoot: runtimeConfig.StateRoot})
		if openErr != nil {
			return nil, openErr
		}
		registration, registrationErr := identityStore.Registration()
		if registrationErr != nil || registration.MachineID != machineID || !filepath.IsAbs(registration.InboxPath) || filepath.Clean(registration.InboxPath) != registration.InboxPath {
			return nil, errors.Join(ErrProductionInvalid, registrationErr)
		}
		inboxPath = registration.InboxPath
		machineRegistration = registration
		// BYOD host enrollment creates the same helper runtime identity used by
		// hosted runtimes. A separate machine-control credential is not part of
		// the host bootstrap contract, so managed SSH must use the renewable
		// helper token and proof instead of requiring a nonexistent file.
		managedSSHIdentity = hostedManagedSSHIdentity{tokens: renewingTokens, proofs: enrollment.ProofSource{StateRoot: runtimeConfig.StateRoot}}
		networkFingerprintSecret, err = identityStore.NetworkFingerprintSecret()
		if err != nil {
			return nil, err
		}
		defer clear(networkFingerprintSecret)
	}
	if runtimeConfig.Profile == runtimeconfig.Hosted {
		machineGeneration, parseErr := strconv.ParseUint(environ("PAPERBOAT_MACHINE_GENERATION"), 10, 63)
		sshPort, portErr := strconv.ParseUint(environ("PAPERBOAT_SSH_PORT"), 10, 16)
		sshUserRaw := environ("PAPERBOAT_SSH_USER")
		sshUser := strings.TrimSpace(sshUserRaw)
		if parseErr != nil || machineGeneration == 0 || portErr != nil || sshPort == 0 || sshUser == "" || sshUser != sshUserRaw {
			return nil, ErrProductionInvalid
		}
		machineRegistration = runtimeidentity.Registration{MachineID: machineID, InstallationGeneration: int64(machineGeneration), SSHUser: sshUser, SSHPort: uint16(sshPort)}
		managedSSHIdentity = hostedManagedSSHIdentity{tokens: renewingTokens, proofs: enrollment.ProofSource{StateRoot: runtimeConfig.StateRoot}}
		if enrollmentClient == nil {
			enrollmentClient, err = enrollment.NewClient(transport, 15*time.Second)
			if err != nil {
				return nil, err
			}
		}
		bootstrap, bootstrapErr := retryHostedControl(ctx, func(attemptCtx context.Context) (enrollment.HostedBootstrap, error) {
			return enrollmentClient.HostedBootstrap(attemptCtx, enrollment.Config{
				ControlURL: controlURL.String(), ControlCAFile: environ("PAPERBOAT_CONTROL_CA_FILE"),
				StateRoot: runtimeConfig.StateRoot,
			})
		})
		if bootstrapErr != nil {
			return nil, fmt.Errorf("hosted bootstrap: %w", bootstrapErr)
		}
		hostedConfig.SetupScript = bootstrap.SetupScript
		hostedConfig.GitToken = bootstrap.SourcePassword
	}
	peerEnrollment, err := peeridentityenrollment.New(peeridentityenrollment.Config{ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, Transport: transport, Timeout: 15 * time.Second}, managedSSHIdentity)
	if err != nil {
		return nil, err
	}
	if err := allowPendingPeerEnrollment(ctx, peerEnrollment); err != nil {
		return nil, err
	}
	var managedEnvironment envinject.EnvironmentSource
	var runtimeProjection *projectionEnvironmentService
	var environmentBootstrap Service
	if environmentInjectionEligible(runtimeConfig.Profile, machineRegistration) {
		runtimeProjection = newProjectionEnvironmentService(runtimeConfig.StateRoot, controlURL, transport, machineRegistration, managedSSHIdentity)
		managedEnvironment, environmentBootstrap = runtimeProjection, runtimeProjection
	}
	fetcher, err := auth.NewHTTPJWKSFetcher(controlURL.ResolveReference(&url.URL{Path: "/.well-known/jwks.json"}).String(), []string{controlURL.Hostname()}, transport)
	if err != nil {
		return nil, err
	}
	cache, err := auth.NewJWKSCache(auth.JWKSConfig{Fetcher: fetcher, Clock: productionClock{}, TTL: 5 * time.Minute, RetainMissing: auth.DefaultRetainMissing, PersistencePath: filepath.Join(runtimeConfig.StateRoot, "authorization", "jwks.json")})
	if err != nil {
		return nil, err
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = cache.Refresh(refreshCtx)
	cancel()
	revocations := auth.NewRevocationCache()
	revocationRefresh, err := newRevocationRefreshService(controlURL.ResolveReference(&url.URL{Path: "/v1/helper-trust/revocations"}).String(), renewingTokens, enrollment.ProofSource{StateRoot: runtimeConfig.StateRoot}, operationID, revocations, transport, 15*time.Second)
	if err != nil {
		return nil, err
	}
	authorizationRefresh := serviceGroup{&jwksRefreshService{cache: cache, interval: time.Minute}, revocationRefresh, newPeerEnrollmentRuntimeService(peerEnrollment, 2*time.Second)}
	if environmentBootstrap != nil {
		authorizationRefresh = append(authorizationRefresh, environmentBootstrap)
	}
	verifier := auth.Verifier{Keys: cache, Clock: productionClock{}, Replays: auth.NewReplayCache(4096, productionClock{}), Revocations: revocations, ClockSkew: 30 * time.Second, RefreshTimeout: 2 * time.Second}
	authorizer, err := NewCredentialAuthorizer(CredentialAuthConfig{Issuer: issuer, EnvironmentID: identity.EnvironmentID, MachineID: machineID, HelperID: identity.HelperID, Verifier: verifier, Revocations: revocations})
	if err != nil {
		return nil, err
	}
	// No transfer is admitted until the authenticated heartbeat supplies policy.
	transferPolicy := &filetransfer.PolicyStore{}
	capabilityController := newDeviceCapabilityController(nil)
	directNetwork := &directNetworkProxy{}
	networkHandler, err := newNetworkChangeHandler(nil, directNetwork, metrics)
	if err != nil {
		return nil, err
	}
	var networkChanges *networkChangeService
	if len(networkFingerprintSecret) >= 32 {
		networkChanges, err = newFingerprintingNetworkChangeService(networkFingerprintSecret, networkHandler.Handle)
	} else {
		networkChanges, err = newNetworkChangeService(networkHandler.Handle)
	}
	if err != nil {
		return nil, err
	}
	if err := networkChanges.ConfigurePortMapping(networkcheck.MappingVerifier{Resolver: net.DefaultResolver, Timeout: 500 * time.Millisecond}); err != nil {
		return nil, err
	}
	connectorService := &dedicatedConnectorService{networkChanges: networkChanges}
	relayRegion := &currentRelayRegion{}
	signalingSubstrate := &signaling.SubstrateManager{}
	regionalMonitor, regionalCache, err := newProductionRegionalMonitor(controlURL, transport, relayRegion.Current, signalingSubstrate, &tls.Config{MinVersion: tls.VersionTLS13}, nil)
	if err != nil {
		return nil, err
	}
	networkHandler.SetObserver(regionalMonitor.NetworkChanged)
	var runtimeObservation *runtimeObservationService
	var availabilityService *availability.Service
	if runtimeConfig.Profile == runtimeconfig.BYOD {
		resolver, resolverErr := availability.NewResolver(controlURL.ResolveReference(&url.URL{Path: "/v1/helper-runtime-policies/resolve"}).String(), renewingTokens, enrollment.ProofSource{StateRoot: runtimeConfig.StateRoot}, operationID, &http.Client{Transport: transport, Timeout: 10 * time.Second})
		if resolverErr != nil {
			return nil, resolverErr
		}
		hostClient, hostErr := newProductionAvailabilityHostClient(5 * time.Second)
		if hostErr != nil {
			return nil, hostErr
		}
		availabilityService, err = availability.NewService(resolver, hostClient, runtimeConfig.Limits.HeartbeatInterval, metrics)
		if err != nil {
			return nil, err
		}
	}
	{
		runtimeEndpoint := controlURL.ResolveReference(&url.URL{Path: "/v1/runtime-observations"}).String()
		scope := environ("PAPERBOAT_RUNTIME_SERVICE_SCOPE")
		if scope != "system" && scope != "user" {
			scope = "unknown"
		}
		capabilities := []string{"file_receive", "preview_launch", "terminal_host", "codex_host", "session_host", "keep_awake"}
		// Presence must not queue behind the work-plane token source. Managed
		// SSH, peer enrollment, and availability all share renewingTokens and a
		// single renewal mutex; when one of those calls is slow, a liveness
		// heartbeat can otherwise miss several 15-second ticks. Give the stable
		// observation service its own renewable source so its bounded request can
		// fail independently while the current identity remains shared on disk.
		observationTokens, observationTokensErr := enrollment.NewRenewingTokenSource(enrollment.RenewingTokenConfig{
			ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, Transport: transport,
			RenewBefore: 5 * time.Minute, Timeout: 15 * time.Second, Clock: func() time.Time { return time.Now().UTC() }, OperationID: operationID, Metrics: metrics,
		})
		if observationTokensErr != nil {
			return nil, observationTokensErr
		}
		sender := &runtimeObservationSender{endpoint: runtimeEndpoint, tokens: observationTokens, proofs: enrollment.ProofSource{StateRoot: runtimeConfig.StateRoot}, operationID: operationID, environmentID: identity.EnvironmentID, machineID: machineID, reporterVersion: version, client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: rejectRuntimePolicyRedirect}, availability: availabilityService, receiptPath: filepath.Join(runtimeConfig.StateRoot, "runtime", "server-heartbeat.json"), installationGeneration: uint64(machineRegistration.InstallationGeneration), workerGeneration: bootState.Generation, osBootID: bootState.OSBootID, lazyBootID: lazyBootID, lazyStartedAt: lazyStartedAt, serviceScope: scope, connector: connectorService, transferPolicy: transferPolicy, capabilitiesController: capabilityController, capabilities: capabilities, relayLatency: regionalCache, relaySuccess: relayRegion}
		if runtimeProjection != nil {
			sender.projection = runtimeProjection
		}
		updaterClient, updaterErr := newProductionUpdaterClient()
		if updaterErr != nil {
			return nil, updaterErr
		}
		sender.updater = updaterClient
		runtimeObservation = &runtimeObservationService{sender: sender, interval: 10 * time.Second, timeout: 10 * time.Second}
	}
	var hostedLifecycle *hosted.Lifecycle
	workspaceRoot := environ("PAPERBOAT_WORKSPACE_ROOT")
	agentShell := "/bin/bash"
	if runtimeConfig.Profile == runtimeconfig.BYOD {
		if strings.TrimSpace(workspaceRoot) == "" {
			workspaceRoot, err = os.UserHomeDir()
			if err != nil {
				return nil, errors.Join(ErrProductionInvalid, errors.New("resolve BYOD home workspace"), err)
			}
		}
		agentShell, err = validatedBYODShell(environ("PAPERBOAT_SHELL"))
		if err != nil {
			return nil, err
		}
	}
	agentEnvironment := []string{"PATH=" + os.Getenv("PATH"), "SHELL=" + agentShell, "TERM=xterm-256color"}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && filepath.IsAbs(home) {
		agentEnvironment = append(agentEnvironment, "HOME="+home)
	}
	shutdownTimeout := 30 * time.Second
	if runtimeConfig.Profile == runtimeconfig.Hosted {
		hostedLifecycle, err = hosted.New(hostedConfig, hosted.ConfigSyncHooks(hostedConfig, environ), nil)
		if err != nil {
			return nil, err
		}
		// Prepare the checkout before constructing the PTY adapter. The adapter
		// validates its root eagerly, while hosted lifecycle owns creating/cloning it.
		if err := hostedLifecycle.Start(ctx); err != nil {
			return nil, err
		}
		if tokenName := environ("PAPERBOAT_GITHUB_TOKEN_ENV"); safeProductionEnvironmentName(tokenName) {
			_ = os.Unsetenv(tokenName)
		}
		workspaceRoot = hostedConfig.VolumeRoot
		shutdownTimeout = hostedConfig.FlushTimeout + 15*time.Second
	} else {
		if err := validateBYODWorkspace(workspaceRoot); err != nil {
			return nil, err
		}
	}
	listen := valueOrRuntime(environ("PAPERBOAT_RUNTIME_LISTEN_ADDRESS"), "127.0.0.1:8080")
	localControlToken, err := writeLocalControlToken(runtimeConfig.StateRoot)
	if err != nil {
		return nil, err
	}
	if err := writeWorkerLocal(runtimeConfig.StateRoot, listen); err != nil {
		return nil, err
	}
	runtimeService := Service(serviceGroup{regionalMonitor, runtimeObservation})
	if availabilityService != nil {
		runtimeService = serviceGroup{availabilityService, regionalMonitor, runtimeObservation}
	}
	previewAssembly, err := newProductionPreviewAssembly(productionPreviewAssemblyConfig{
		ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, MachineID: machineID, InstallationGeneration: machineRegistration.InstallationGeneration, BootID: lazyBootID,
		LocalControlToken: localControlToken, Transport: transport, RunContext: ctx,
	})
	if err != nil {
		return nil, err
	}
	codexManager, err := codexsession.New(codexsession.Config{
		StateRoot: filepath.Join(runtimeConfig.StateRoot, "codex"), WorkspaceRoot: workspaceRoot,
		Environment: agentEnvironment, ManagedEnvironment: managedEnvironmentFunction(managedEnvironment),
		CodexPath: valueOrRuntime(environ("PAPERBOAT_CODEX_PATH"), "codex"), MaxSessions: 4,
	})
	if err != nil {
		return nil, err
	}
	managedSSHHost, managedSSHService, err := productionManagedSSH(ctx, controlURL.String(), transport, machineRegistration, managedSSHIdentity, uint64(bootState.Generation))
	if err != nil {
		return nil, err
	}
	transferKeys, err := transfercrypto.NewKeyVault(clientconfig.FileSecretStore{Dir: filepath.Join(runtimeConfig.StateRoot, "transfer-keys")})
	if err != nil {
		return nil, err
	}
	dependencies := HostDependencies{Authorizer: authorizer, AuthorizationService: authorizationRefresh, Connector: connectorService, PreviewDispatcher: previewAssembly, PreviewRecovery: previewAssembly, PreviewOwnerSessions: previewAssembly.OwnerSessionLeases(), RuntimeObservationService: runtimeService, ManagedEnvironment: managedEnvironment, Metrics: metrics, CodexSessions: codexManager, LocalControlToken: localControlToken, ManagedSSH: managedSSHHost, ManagedSSHService: managedSSHService, TransferKeys: transferKeys, Capabilities: capabilityController}
	nativePrivateValidators := []productionNativePrivateValidator{previewAssembly.dispatcher}
	if tunnelProvider == nil {
		tunnelEnrollment, enrollmentErr := newProductionTunnelEnrollmentService(controlURL.String(), runtimeConfig.StateRoot, machineID, localControlToken, transport)
		if enrollmentErr != nil {
			return nil, errors.Join(ErrProductionInvalid, enrollmentErr)
		}
		dependencies.TunnelEnrollment = tunnelEnrollment
		dependencies.TunnelEnrollmentLifecycle = platformTunnelEnrollmentLifecycle(tunnelEnrollment)
		dependencies.TunnelManager = tunnelEnrollment
		connectorService.status = tunnelEnrollment.Status
		networkHandler.SetCanonical(tunnelEnrollment)
	} else {
		tunnelAssembly, assemblyErr := productionTunnelAssembly(ctx, tunnelProvider, ProductionTunnelAssemblyInputs{
			StateRoot: runtimeConfig.StateRoot, ControlURL: controlURL.String(), ControlTransport: transport,
			EnvironmentID: identity.EnvironmentID, MachineID: machineID,
			InstallationGeneration: uint64(machineRegistration.InstallationGeneration), Metrics: metrics,
		})
		if assemblyErr != nil {
			return nil, errors.Join(ErrProductionInvalid, assemblyErr)
		}
		dependencies.TunnelManager = tunnelAssembly
		connectorService.status = tunnelAssembly.ConnectorStatus
		nativePrivateValidators = append(nativePrivateValidators, tunnelAssembly.Manager.Manager)
		updateGate, gateErr := tunnelmanager.NewUpdateGate(tunnelmanager.UpdateGateConfig{MachineID: machineID, Manager: tunnelAssembly.Manager.Manager, StatePath: filepath.Join(runtimeConfig.StateRoot, "updates", "deployment-gate.json")})
		if gateErr != nil {
			return nil, errors.Join(ErrProductionInvalid, gateErr)
		}
		dependencies.UpdateGate = updateGate
		networkHandler.SetCanonical(tunnelAssembly)
	}
	if managedSSHIdentity != nil {
		dependencies.NativePeerFactory = func(serve func(net.Conn) error, transferHandler, codexHandler http.Handler) (Service, error) {
			return newProductionNativePeerService(productionNativePeerConfig{controlURL: controlURL.String(), issuer: issuer, stateRoot: runtimeConfig.StateRoot, machineID: machineID, generation: uint64(machineRegistration.InstallationGeneration), transport: transport, identity: managedSSHIdentity, keys: cache, authorizer: authorizer, serve: serve, transfer: transferHandler, codex: codexHandler, ssh: managedSSHHost, privateCurrent: productionNativeCurrent(nativePrivateValidators...), privateDial: productionNativePrivateDial})
		}
	}
	if runtimeConfig.Profile == runtimeconfig.Hosted {
		dependencies.HostedLifecycle = hostedLifecycle
	}
	result, err := NewHost(ctx, HostConfig{Runtime: runtimeConfig, ListenAddress: listen, WorkspaceRoot: workspaceRoot, ShellPath: agentShell, AgentEnvironment: agentEnvironment, EnvironmentID: identity.EnvironmentID, MachineID: machineID, InboxPath: inboxPath, ShutdownTimeout: shutdownTimeout, RecoveryExitSignal: recoveryExitSignal, FileTransferPolicy: transferPolicy}, dependencies)
	if err == nil {
		capabilityController.SetReconciler(result.reconcileDeviceCapabilities)
	}
	return result, err
}

func environmentInjectionEligible(profile runtimeconfig.Profile, registration runtimeidentity.Registration) bool {
	if registration.SetupMode != "host" {
		return false
	}
	return profile == runtimeconfig.BYOD || profile == runtimeconfig.Hosted
}

func managedEnvironmentFunction(source envinject.EnvironmentSource) func() ([]string, error) {
	if source == nil {
		return nil
	}
	return source.Environment
}

type managedSSHIdentitySource interface {
	Token(context.Context) (string, error)
	Proof(context.Context, string, string, string, []byte) ([]byte, error)
}

type managedSSHIdentityTokens interface {
	Token(context.Context) (string, error)
}

type managedSSHIdentityProofs interface {
	Proof(context.Context, string, string, string, []byte) ([]byte, error)
}

type hostedManagedSSHIdentity struct {
	tokens managedSSHIdentityTokens
	proofs managedSSHIdentityProofs
}

func (s hostedManagedSSHIdentity) Token(ctx context.Context) (string, error) {
	return s.tokens.Token(ctx)
}

func (s hostedManagedSSHIdentity) Proof(ctx context.Context, operationID, method, path string, body []byte) ([]byte, error) {
	return s.proofs.Proof(ctx, operationID, method, path, body)
}

type managedSSHControlClient interface {
	ObserveManagedSSHHostKeys(context.Context, string, string, string, string, uint64, uint64, []string, []byte) (clientapi.ManagedSSHHostKeySet, error)
	ManagedSSHAuthorizedKeys(context.Context, string, string, uint64, []byte) (clientapi.ManagedSSHAuthorizedKeys, error)
}

func productionManagedSSHUnix(ctx context.Context, controlURL string, transport http.RoundTripper, registration runtimeidentity.Registration, identitySource managedSSHIdentitySource, observationGeneration uint64) (*managedssh.Host, Service, error) {
	if registration.MachineID == "" || registration.InstallationGeneration < 1 || registration.SSHPort == 0 || registration.SSHUser == "" || identitySource == nil {
		return nil, nil, nil
	}
	host, err := managedssh.NewHost(managedssh.HostConfig{MaxStreams: 32, ProbeTimeout: 3 * time.Second, DialTimeout: 10 * time.Second})
	if err != nil {
		return nil, nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err = host.ReconcileTarget(probeCtx, uint64(registration.InstallationGeneration), registration.SSHPort)
	cancel()
	if err != nil {
		return nil, nil, errors.Join(ErrManagedSSHUnavailable, err)
	}
	paths := existingSSHHostPublicKeyPathsUnix()
	if len(paths) == 0 {
		return nil, nil, errors.Join(ErrManagedSSHUnavailable, errors.New("no SSH host public key is published"))
	}
	inventory, err := managedssh.ReadHostPublicKeys(paths, 0)
	if err != nil {
		return nil, nil, errors.Join(ErrManagedSSHUnavailable, err)
	}
	if observationGeneration == 0 {
		return nil, nil, errors.New("managed SSH observation generation is unavailable")
	}
	publicKeys := make([]string, len(inventory.Keys))
	for index := range inventory.Keys {
		publicKeys[index] = inventory.Keys[index].PublicKey
	}
	setID := "sshks_" + hex.EncodeToString(inventory.Fingerprint[:16])
	client := clientapi.New(controlURL, clientconfig.Credential{}, &http.Client{Transport: transport, Timeout: 15 * time.Second})
	account, err := user.Lookup(registration.SSHUser)
	if err != nil || !filepath.IsAbs(account.HomeDir) {
		return nil, nil, errors.Join(ErrManagedSSHUnavailable, errors.New("managed SSH operating-system user is unavailable"), err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, nil, errors.Join(ErrManagedSSHUnavailable, errors.New("managed SSH operating-system user identifier is invalid"), err)
	}
	reconciler := &managedSSHKeyReconciler{
		client: client, identity: identitySource, registration: registration,
		workerGeneration: observationGeneration, setID: setID, publicKeys: publicKeys,
		home: account.HomeDir, ownerUID: uint32(uid), interval: 30 * time.Second, timeout: 10 * time.Second,
	}
	return host, reconciler, nil
}

func managedSSHInitialOperationIDs(registration runtimeidentity.Registration, observationGeneration uint64, fingerprint [32]byte) (string, string) {
	// Machine proofs cap operation IDs at 128 bytes. Hash the complete durable
	// identity rather than embedding it verbatim so IDs remain bounded while
	// still changing for every machine, installation, observation and host-key
	// fingerprint tuple.
	digest := sha256.Sum256([]byte(registration.MachineID + "\x00" + strconv.FormatUint(uint64(registration.InstallationGeneration), 10) + "\x00" + strconv.FormatUint(observationGeneration, 10) + "\x00" + hex.EncodeToString(fingerprint[:])))
	suffix := base64.RawURLEncoding.EncodeToString(digest[:])
	return "managed-ssh-observe-" + suffix, "managed-ssh-keys-" + suffix
}

// reconcileManagedSSHAuthority retains the original operation-ID helper for
// callers that do not have a host-key inventory. Production host setup uses
// reconcileManagedSSHAuthorityWithFingerprint below so retries are scoped to
// the exact persisted host-key set.
func reconcileManagedSSHAuthority(ctx context.Context, client managedSSHControlClient, identitySource managedSSHIdentitySource, registration runtimeidentity.Registration, observationGeneration uint64, setID string, publicKeys []string) (clientapi.ManagedSSHAuthorizedKeys, bool, error) {
	return reconcileManagedSSHAuthorityWithOperations(ctx, client, identitySource, registration, observationGeneration, setID, publicKeys,
		"managed-ssh-observe-"+registration.MachineID+"-"+strconv.FormatUint(uint64(registration.InstallationGeneration), 10)+"-"+strconv.FormatUint(observationGeneration, 10),
		"managed-ssh-keys-"+registration.MachineID+"-"+strconv.FormatUint(uint64(registration.InstallationGeneration), 10)+"-"+strconv.FormatUint(observationGeneration, 10))
}

func reconcileManagedSSHAuthorityWithFingerprint(ctx context.Context, client managedSSHControlClient, identitySource managedSSHIdentitySource, registration runtimeidentity.Registration, observationGeneration uint64, setID string, fingerprint [32]byte, publicKeys []string) (clientapi.ManagedSSHAuthorizedKeys, bool, error) {
	observeOperationID, keyOperationID := managedSSHInitialOperationIDs(registration, observationGeneration, fingerprint)
	return reconcileManagedSSHAuthorityWithOperations(ctx, client, identitySource, registration, observationGeneration, setID, publicKeys,
		observeOperationID, keyOperationID)
}

func reconcileManagedSSHAuthorityWithOperations(ctx context.Context, client managedSSHControlClient, identitySource managedSSHIdentitySource, registration runtimeidentity.Registration, observationGeneration uint64, setID string, publicKeys []string, observeOperationID, keyOperationID string) (clientapi.ManagedSSHAuthorizedKeys, bool, error) {
	if ctx == nil || client == nil || identitySource == nil || registration.MachineID == "" || registration.InstallationGeneration < 1 || observationGeneration == 0 || setID == "" || len(publicKeys) == 0 {
		return clientapi.ManagedSSHAuthorizedKeys{}, false, ErrProductionInvalid
	}
	body, err := json.Marshal(map[string]any{"set_id": setID, "observation_generation": observationGeneration, "public_keys": publicKeys})
	if err != nil {
		return clientapi.ManagedSSHAuthorizedKeys{}, false, err
	}
	path := "/v1/machines/" + registration.MachineID + "/ssh-host-keys"
	proof, err := identitySource.Proof(ctx, observeOperationID, http.MethodPut, path, body)
	if err != nil {
		return clientapi.ManagedSSHAuthorizedKeys{}, false, err
	}
	identityCredential, err := identitySource.Token(ctx)
	if err != nil {
		return clientapi.ManagedSSHAuthorizedKeys{}, false, err
	}
	set, err := client.ObserveManagedSSHHostKeys(ctx, registration.MachineID, identityCredential, observeOperationID, setID, uint64(registration.InstallationGeneration), observationGeneration, publicKeys, proof)
	if err != nil {
		return clientapi.ManagedSSHAuthorizedKeys{}, false, err
	}
	if set.State != "active" {
		return clientapi.ManagedSSHAuthorizedKeys{}, false, nil
	}
	keyBody := []byte("{}")
	keyPath := "/v1/machines/" + registration.MachineID + "/ssh-authorized-keys"
	keyProof, err := identitySource.Proof(ctx, keyOperationID, http.MethodPost, keyPath, keyBody)
	if err != nil {
		return clientapi.ManagedSSHAuthorizedKeys{}, false, err
	}
	identityCredential, err = identitySource.Token(ctx)
	if err != nil {
		return clientapi.ManagedSSHAuthorizedKeys{}, false, err
	}
	keySet, err := client.ManagedSSHAuthorizedKeys(ctx, registration.MachineID, identityCredential, uint64(registration.InstallationGeneration), keyProof)
	if err != nil {
		return clientapi.ManagedSSHAuthorizedKeys{}, false, err
	}
	return keySet, true, nil
}

func existingSSHHostPublicKeyPathsUnix() []string {
	candidates := []string{
		"/etc/ssh/ssh_host_ed25519_key.pub",
		"/etc/ssh/ssh_host_ecdsa_key.pub",
		"/etc/ssh/ssh_host_rsa_key.pub",
	}
	result := make([]string, 0, len(candidates))
	for _, path := range candidates {
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			result = append(result, path)
		}
	}
	return result
}

func shouldRunClientCoordinator(registrationMode, installedMode string) bool {
	return registrationMode == "client" && installedMode != "host"
}

func newProductionClientCoordinator(ctx context.Context, version string, environ func(string) string, runtimeConfig runtimeconfig.Config, bootState workerBootState, recoveryExitSignal string, metrics *observability.Registry, registration runtimeidentity.Registration, tunnelProvider ProductionTunnelAssemblyProvider) (*Host, error) {
	controlURL, err := validatedControlURL(environ("PAPERBOAT_CONTROL_URL"))
	if err != nil || registration.MachineID != environ("PAPERBOAT_MACHINE_ID") || registration.EnvironmentID == "" {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	issuer := strings.TrimRight(valueOrRuntime(environ("PAPERBOAT_CONTROL_ISSUER"), controlURL.String()), "/")
	transport, err := productionTransport(environ("PAPERBOAT_CONTROL_CA_FILE"), environ)
	if err != nil {
		return nil, err
	}
	operationID := func() (string, error) {
		value := make([]byte, 16)
		if _, err := rand.Read(value); err != nil {
			return "", err
		}
		return "op_client_" + hex.EncodeToString(value), nil
	}
	identity, err := enrollment.LoadRuntimeIdentityForRenewal(runtimeConfig.StateRoot, time.Now().UTC())
	if err != nil || identity.MachineID != registration.MachineID || identity.EnvironmentID != registration.EnvironmentID {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	// Client-mode runtimes are enrolled helpers, not host machine-control
	// principals. Use their renewable runtime identity for control-plane calls,
	// exactly as the shared host runtime does. A machine-control source rejects
	// client registrations and prevents hostd from ever becoming ready.
	runtimeTokens, err := enrollment.NewRenewingTokenSource(enrollment.RenewingTokenConfig{
		ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, Transport: transport,
		RenewBefore: 10 * time.Minute, Timeout: 15 * time.Second, Clock: func() time.Time { return time.Now().UTC() }, OperationID: operationID, Metrics: metrics,
	})
	if err != nil {
		return nil, err
	}
	runtimeProofs := enrollment.ProofSource{StateRoot: runtimeConfig.StateRoot}
	runtimeIdentity := hostedManagedSSHIdentity{tokens: runtimeTokens, proofs: runtimeProofs}
	peerEnrollment, err := peeridentityenrollment.New(peeridentityenrollment.Config{ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, Transport: transport, Timeout: 15 * time.Second}, runtimeIdentity)
	if err != nil {
		return nil, err
	}
	if err := allowPendingPeerEnrollment(ctx, peerEnrollment); err != nil {
		return nil, err
	}
	fetcher, err := auth.NewHTTPJWKSFetcher(controlURL.ResolveReference(&url.URL{Path: "/.well-known/jwks.json"}).String(), []string{controlURL.Hostname()}, transport)
	if err != nil {
		return nil, err
	}
	cache, err := auth.NewJWKSCache(auth.JWKSConfig{Fetcher: fetcher, Clock: productionClock{}, TTL: 5 * time.Minute, RetainMissing: auth.DefaultRetainMissing, PersistencePath: filepath.Join(runtimeConfig.StateRoot, "authorization", "jwks.json")})
	if err != nil {
		return nil, err
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = cache.Refresh(refreshCtx)
	cancel()
	revocations := auth.NewRevocationCache()
	revocationRefresh, err := newRevocationRefreshService(controlURL.ResolveReference(&url.URL{Path: "/v1/helper-trust/revocations"}).String(), runtimeTokens, runtimeProofs, operationID, revocations, transport, 15*time.Second)
	if err != nil {
		return nil, err
	}
	authorizationRefresh := serviceGroup{&jwksRefreshService{cache: cache, interval: time.Minute}, revocationRefresh, newPeerEnrollmentRuntimeService(peerEnrollment, 2*time.Second)}
	verifier := auth.Verifier{Keys: cache, Clock: productionClock{}, Replays: auth.NewReplayCache(4096, productionClock{}), Revocations: revocations, ClockSkew: 30 * time.Second, RefreshTimeout: 2 * time.Second}
	authorizer, err := NewCredentialAuthorizer(CredentialAuthConfig{Issuer: issuer, EnvironmentID: registration.EnvironmentID, MachineID: registration.MachineID, HelperID: identity.HelperID, Verifier: verifier, Revocations: revocations})
	if err != nil {
		return nil, err
	}
	transferPolicy := &filetransfer.PolicyStore{}
	capabilityController := newDeviceCapabilityController(nil)
	directNetwork := &directNetworkProxy{}
	networkHandler, err := newNetworkChangeHandler(nil, directNetwork, metrics)
	if err != nil {
		return nil, err
	}
	identityStore, err := runtimeidentity.Open(runtimeidentity.Config{StateRoot: runtimeConfig.StateRoot})
	if err != nil {
		return nil, err
	}
	networkFingerprintSecret, err := identityStore.NetworkFingerprintSecret()
	if err != nil {
		return nil, err
	}
	defer clear(networkFingerprintSecret)
	networkChanges, err := newFingerprintingNetworkChangeService(networkFingerprintSecret, networkHandler.Handle)
	if err != nil {
		return nil, err
	}
	if err := networkChanges.ConfigurePortMapping(networkcheck.MappingVerifier{Resolver: net.DefaultResolver, Timeout: 500 * time.Millisecond}); err != nil {
		return nil, err
	}
	connectorService := &dedicatedConnectorService{networkChanges: networkChanges}
	relayRegion := &currentRelayRegion{}
	signalingSubstrate := &signaling.SubstrateManager{}
	regionalMonitor, regionalCache, err := newProductionRegionalMonitor(controlURL, transport, relayRegion.Current, signalingSubstrate, &tls.Config{MinVersion: tls.VersionTLS13}, nil)
	if err != nil {
		return nil, err
	}
	networkHandler.SetObserver(regionalMonitor.NetworkChanged)
	scope := environ("PAPERBOAT_RUNTIME_SERVICE_SCOPE")
	if scope != "system" && scope != "user" {
		scope = "unknown"
	}
	sender := &runtimeObservationSender{endpoint: controlURL.ResolveReference(&url.URL{Path: "/v1/runtime-observations"}).String(), tokens: runtimeTokens, proofs: runtimeProofs, operationID: operationID, environmentID: registration.EnvironmentID, machineID: registration.MachineID, reporterVersion: version, client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: rejectRuntimePolicyRedirect}, receiptPath: filepath.Join(runtimeConfig.StateRoot, "runtime", "server-heartbeat.json"), installationGeneration: uint64(registration.InstallationGeneration), workerGeneration: bootState.Generation, osBootID: bootState.OSBootID, serviceScope: scope, connector: connectorService, transferPolicy: transferPolicy, capabilitiesController: capabilityController, capabilities: []string{"file_receive", "preview_launch"}, relayLatency: regionalCache, relaySuccess: relayRegion}
	updaterClient, updaterErr := newProductionUpdaterClient()
	if updaterErr != nil {
		return nil, updaterErr
	}
	sender.updater = updaterClient
	observation := &runtimeObservationService{sender: sender, interval: runtimeConfig.Limits.HeartbeatInterval, timeout: 10 * time.Second}
	listen := valueOrRuntime(environ("PAPERBOAT_RUNTIME_LISTEN_ADDRESS"), "127.0.0.1:8080")
	localControlToken, err := writeLocalControlToken(runtimeConfig.StateRoot)
	if err != nil {
		return nil, err
	}
	if err := writeWorkerLocal(runtimeConfig.StateRoot, listen); err != nil {
		return nil, err
	}
	transferKeys, err := transfercrypto.NewKeyVault(clientconfig.FileSecretStore{Dir: filepath.Join(runtimeConfig.StateRoot, "transfer-keys")})
	if err != nil {
		return nil, err
	}
	var nativePrivateValidators []productionNativePrivateValidator
	nativePeerFactory := func(serve func(net.Conn) error, transferHandler, codexHandler http.Handler) (Service, error) {
		return newProductionNativePeerService(productionNativePeerConfig{controlURL: controlURL.String(), issuer: issuer, stateRoot: runtimeConfig.StateRoot, machineID: registration.MachineID, generation: uint64(registration.InstallationGeneration), transport: transport, identity: runtimeIdentity, keys: cache, authorizer: authorizer, serve: serve, transfer: transferHandler, codex: codexHandler, privateCurrent: productionNativeCurrent(nativePrivateValidators...), privateDial: productionNativePrivateDial})
	}
	dependencies := HostDependencies{Authorizer: authorizer, AuthorizationService: authorizationRefresh, Connector: connectorService, PreviewRecovery: nil, RuntimeObservationService: serviceGroup{regionalMonitor, observation}, Metrics: metrics, LocalControlToken: localControlToken, TransferKeys: transferKeys, NativePeerFactory: nativePeerFactory, Capabilities: capabilityController}
	if tunnelProvider == nil {
		tunnelEnrollment, enrollmentErr := newProductionTunnelEnrollmentService(controlURL.String(), runtimeConfig.StateRoot, registration.MachineID, localControlToken, transport)
		if enrollmentErr != nil {
			return nil, errors.Join(ErrProductionInvalid, enrollmentErr)
		}
		dependencies.TunnelEnrollment = tunnelEnrollment
		dependencies.TunnelEnrollmentLifecycle = platformTunnelEnrollmentLifecycle(tunnelEnrollment)
		dependencies.TunnelManager = tunnelEnrollment
		connectorService.status = tunnelEnrollment.Status
		networkHandler.SetCanonical(tunnelEnrollment)
	} else {
		tunnelAssembly, assemblyErr := productionTunnelAssembly(ctx, tunnelProvider, ProductionTunnelAssemblyInputs{
			StateRoot: runtimeConfig.StateRoot, ControlURL: controlURL.String(), ControlTransport: transport,
			EnvironmentID: registration.EnvironmentID, MachineID: registration.MachineID,
			InstallationGeneration: uint64(registration.InstallationGeneration), Metrics: metrics,
		})
		if assemblyErr != nil {
			return nil, errors.Join(ErrProductionInvalid, assemblyErr)
		}
		dependencies.TunnelManager = tunnelAssembly
		connectorService.status = tunnelAssembly.ConnectorStatus
		nativePrivateValidators = append(nativePrivateValidators, tunnelAssembly.Manager.Manager)
		updateGate, gateErr := tunnelmanager.NewUpdateGate(tunnelmanager.UpdateGateConfig{MachineID: registration.MachineID, Manager: tunnelAssembly.Manager.Manager, StatePath: filepath.Join(runtimeConfig.StateRoot, "updates", "deployment-gate.json")})
		if gateErr != nil {
			return nil, errors.Join(ErrProductionInvalid, gateErr)
		}
		dependencies.UpdateGate = updateGate
		networkHandler.SetCanonical(tunnelAssembly)
	}
	result, err := NewClientCoordinator(ctx, HostConfig{Runtime: runtimeConfig, ListenAddress: listen, WorkspaceRoot: registration.InboxPath, EnvironmentID: registration.EnvironmentID, MachineID: registration.MachineID, InboxPath: registration.InboxPath, ShutdownTimeout: 30 * time.Second, RecoveryExitSignal: recoveryExitSignal, FileTransferPolicy: transferPolicy}, dependencies)
	if err == nil {
		capabilityController.SetReconciler(result.reconcileDeviceCapabilities)
	}
	return result, err
}

type peerEnrollmentEnsurer interface {
	Ensure(context.Context) error
}

type peerEnrollmentRuntimeService struct {
	enrollment peerEnrollmentEnsurer
	interval   time.Duration
	cancel     context.CancelFunc
	done       chan struct{}
}

func newPeerEnrollmentRuntimeService(enrollment peerEnrollmentEnsurer, interval time.Duration) *peerEnrollmentRuntimeService {
	return &peerEnrollmentRuntimeService{enrollment: enrollment, interval: interval, done: make(chan struct{})}
}

func (s *peerEnrollmentRuntimeService) Start(ctx context.Context) error {
	if s == nil || s.enrollment == nil || s.interval <= 0 || ctx == nil || s.cancel != nil {
		return ErrProductionInvalid
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go func() {
		defer close(s.done)
		_ = waitForPeerEnrollment(runCtx, s.enrollment, s.interval)
	}()
	return nil
}

func (s *peerEnrollmentRuntimeService) Shutdown(ctx context.Context) error {
	if s == nil || ctx == nil || s.cancel == nil {
		return ErrProductionInvalid
	}
	s.cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func allowPendingPeerEnrollment(ctx context.Context, enrollment peerEnrollmentEnsurer) error {
	err := enrollment.Ensure(ctx)
	if !errors.Is(err, peeridentityenrollment.ErrPending) {
		return err
	}
	var pending *peeridentityenrollment.PendingError
	if errors.As(err, &pending) {
		slog.Warn("machine endpoint approval pending; relay connectivity remains available", "request_id", pending.RequestID, "safety_code", pending.SafetyCode, "expires_at", pending.ExpiresAt)
	}
	return nil
}

func runtimeEnvironmentEndpoint(stateRoot string) (runtimeidentity.PeerEndpoint, error) {
	store, err := runtimeidentity.Open(runtimeidentity.Config{StateRoot: stateRoot})
	if err != nil {
		return runtimeidentity.PeerEndpoint{}, err
	}
	return store.PeerEndpoint()
}

func waitEnvironmentBootstrap(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func productionEnvironmentKeySource(registration runtimeidentity.Registration) environmentkey.Source {
	if runtime.GOOS == "linux" {
		return environmentkey.SystemdCredentialSource{Generation: uint64(registration.InstallationGeneration), MachineID: registration.MachineID}
	}
	return environmentkey.KeyringSource{Store: clientconfig.KeyringStore{}, MachineID: registration.MachineID, Generation: uint64(registration.InstallationGeneration), NotFound: func(err error) bool { return errors.Is(err, clientconfig.ErrSecretNotFound) }}
}

func waitForPeerEnrollment(ctx context.Context, enrollment peerEnrollmentEnsurer, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	reported := false
	for {
		err := enrollment.Ensure(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, peeridentityenrollment.ErrPending) {
			return err
		}
		if !reported {
			var pending *peeridentityenrollment.PendingError
			if errors.As(err, &pending) {
				slog.Warn("machine endpoint approval pending", "request_id", pending.RequestID, "safety_code", pending.SafetyCode, "expires_at", pending.ExpiresAt)
			}
			reported = true
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func writeLocalControlToken(stateRoot string) (string, error) {
	if !filepath.IsAbs(stateRoot) {
		return "", ErrProductionInvalid
	}
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(value)
	directory := filepath.Join(stateRoot, "runtime")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(directory, "local-control-token")
	if err := atomicfile.Write(path, []byte(token), atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}); err != nil {
		return "", err
	}
	return token, nil
}

func writeWorkerLocal(stateRoot, listenAddress string) error {
	body, err := json.Marshal(struct {
		Schema        string `json:"schema"`
		ListenAddress string `json:"listen_address"`
	}{"paperboat.worker-local/v1", listenAddress})
	if err != nil {
		return err
	}
	path := filepath.Join(stateRoot, "runtime", "worker-local.json")
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}

func retryHostedControl[T any](ctx context.Context, operation func(context.Context) (T, error)) (T, error) {
	var zero T
	if operation == nil {
		return zero, ErrProductionInvalid
	}
	backoff := time.Second
	for {
		result, err := operation(ctx)
		if err == nil {
			return result, nil
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}
}

func validatedBYODShellUnix(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/bin/sh"
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.Join(ErrProductionInvalid, errors.New("BYOD shell must be an absolute canonical path"))
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.Join(ErrProductionInvalid, errors.New("BYOD shell must resolve to an absolute canonical path"))
	}
	resolved := path
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err = filepath.EvalSymlinks(path)
	}
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", errors.Join(ErrProductionInvalid, errors.New("BYOD shell must resolve to an absolute canonical path"))
	}
	info, err = os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.Join(ErrProductionInvalid, errors.New("BYOD shell must be an executable regular file"))
	}
	return resolved, nil
}

func validateBYODWorkspaceUnix(root string) error {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.Join(ErrProductionInvalid, errors.New("BYOD workspace must be an absolute canonical path"))
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.Join(ErrProductionInvalid, errors.New("BYOD workspace must be an existing non-symlink directory"))
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return errors.Join(ErrProductionInvalid, errors.New("BYOD workspace symlink resolution is not permitted"))
	}
	return nil
}

type runtimeObservationService struct {
	mu       sync.Mutex
	sender   *runtimeObservationSender
	interval time.Duration
	timeout  time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

func (s *runtimeObservationService) Start(ctx context.Context) error {
	if s.sender == nil || s.interval <= 0 || s.timeout <= 0 {
		return ErrProductionInvalid
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	initial, cancel := context.WithTimeout(ctx, s.timeout)
	err := s.sender.Send(initial)
	cancel()
	if err != nil {
		// Presence is the required part of this service. A first observation
		// can fail while credentials, DNS, or an auxiliary local store is
		// recovering. Do not let one transient failure make the optional
		// component disappear permanently: install the stable loop and let its
		// bounded sends retry on the normal heartbeat cadence.
		slog.Warn("initial runtime observation failed; continuing heartbeat retries", "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	// Service.Start's context bounds startup only. The stable daemon owns the
	// accepted service until it calls Shutdown; tying the loop to the caller's
	// startup context silently stops machine presence after successful startup
	// on supervisors that cancel that context. Partial starts are still safe
	// because hostd always invokes Shutdown for an accepted or failed component.
	runCtx, stop := context.WithCancel(context.Background())
	s.cancel, s.done = stop, make(chan struct{})
	go s.loop(runCtx, s.done)
	return nil
}

func (s *runtimeObservationService) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendCtx, cancel := context.WithTimeout(ctx, s.timeout)
			if err := s.sender.Send(sendCtx); err != nil {
				slog.Warn("runtime observation failed", "error", err)
			}
			cancel()
		}
	}
}

func (s *runtimeObservationService) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	final, stop := context.WithTimeout(ctx, s.timeout)
	err := s.sender.Send(final)
	stop()
	return err
}

type runtimeObservationSender struct {
	endpoint, environmentID, machineID, reporterVersion string
	tokens                                              interface {
		Token(context.Context) (string, error)
	}
	proofs interface {
		Proof(context.Context, string, string, string, []byte) ([]byte, error)
	}
	operationID  func() (string, error)
	client       *http.Client
	availability interface {
		Observation() *availability.Observation
	}
	updater interface {
		Status(context.Context) (updated.ControlResponse, error)
	}
	environment interface {
		NextObservation(time.Time) (envinject.Observation, error)
		Apply(context.Context, envinject.Bundle) error
		BindingState() envinject.BindingState
	}
	projection interface {
		NextObservation(time.Time) (envinject.ProjectionObservation, error)
		Apply(context.Context, envinject.ProjectionBundle) error
		BindingState() envinject.BindingState
	}
	onEnvironmentObservation func()
	receiptPath              string
	installationGeneration   uint64
	workerGeneration         uint64
	osBootID                 string
	lazyBootID               string
	lazyStartedAt            time.Time
	serviceScope             string
	connector                interface{ Status() connector.Status }
	transferPolicy           *filetransfer.PolicyStore
	capabilities             []string
	capabilitiesController   *deviceCapabilityController
	relayLatency             interface {
		Vector(time.Time) relayselection.Vector
	}
	relaySuccess    interface{ Success() (string, time.Time) }
	relayLatencyMu  sync.Mutex
	relayLatencyGen uint64
}

func (s *runtimeObservationSender) Send(ctx context.Context) error {
	now := time.Now().UTC()
	var environmentObservation *envinject.Observation
	var projectionObservation *envinject.ProjectionObservation
	environmentObservationSent := false
	if s.environment != nil {
		observation, err := s.environment.NextObservation(now)
		if err != nil {
			if !errors.Is(err, envinject.ErrNotReady) {
				// ENV is an auxiliary observation. Its local encrypted store can
				// be unavailable or revoked while machine presence remains valid;
				// never suppress the authenticated heartbeat in that case.
				slog.Warn("runtime environment observation failed", "error", err)
			}
		} else {
			environmentObservation = &observation
			environmentObservationSent = true
		}
	}
	if s.projection != nil {
		observation, err := s.projection.NextObservation(now)
		if err == nil {
			projectionObservation = &observation
		}
	}
	var observationPayload any
	if projectionObservation != nil {
		observationPayload = projectionObservation
	} else if environmentObservation != nil {
		observationPayload = environmentObservation
	}
	environmentReady := s.projection != nil && s.projection.BindingState() == envinject.BindingActive || environmentObservation != nil && s.environment.BindingState() == envinject.BindingActive
	relayLatency := s.nextRelayLatency(now)
	availabilityState := availabilityObservation(s.availability)
	var updaterState *updated.ControlResponse
	var updaterErr error
	if s.updater != nil {
		status, err := s.updater.Status(ctx)
		if err != nil {
			updaterErr = err
		} else {
			updaterState = &status
		}
	}
	body, err := json.Marshal(struct {
		LazyRuntime        *lazyRuntimeObservation         `json:"lazy_runtime,omitempty"`
		EnvironmentID      string                          `json:"environment_id"`
		ResourceID         string                          `json:"resource_id"`
		ReporterVersion    string                          `json:"reporter_version"`
		SampledAt          time.Time                       `json:"sampled_at"`
		Environment        any                             `json:"environment,omitempty"`
		Availability       *availability.Observation       `json:"availability,omitempty"`
		RuntimeDiagnostics *runtimeDiagnosticsObservation  `json:"runtime_diagnostics,omitempty"`
		RelayLatency       *runtimeRelayLatencyObservation `json:"relay_latency,omitempty"`
		Update             *runtimeUpdateObservation       `json:"update,omitempty"`
		DeviceCapabilities *deviceCapabilitiesObservation  `json:"device_capabilities,omitempty"`
	}{
		LazyRuntime:        s.lazyRuntimeObservation(),
		EnvironmentID:      s.environmentID,
		ResourceID:         s.machineID,
		ReporterVersion:    s.reporterVersion,
		SampledAt:          now,
		Environment:        observationPayload,
		Availability:       availabilityState,
		RuntimeDiagnostics: s.runtimeDiagnostics(now, environmentReady),
		RelayLatency:       relayLatency,
		Update:             s.updateObservationFrom(now, availabilityState, updaterState, updaterErr),
		DeviceCapabilities: s.capabilitiesController.Observation(now),
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	token, err := s.tokens.Token(ctx)
	if err != nil {
		return err
	}
	operationID, err := s.operationID()
	if err != nil {
		return err
	}
	proof, err := s.proofs.Proof(ctx, operationID, http.MethodPost, "/v1/runtime-observations", body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	defer func() {
		for index := range responseBody {
			responseBody[index] = 0
		}
	}()
	if readErr != nil || len(responseBody) > 8<<20 {
		return errors.New("runtime observation response is invalid")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("runtime observation rejected with status %d", response.StatusCode)
	}
	if s.transferPolicy != nil {
		if err := applyRuntimeTransferPolicy(responseBody, s.transferPolicy); err != nil {
			return err
		}
	}
	if s.capabilitiesController != nil {
		if err := applyRuntimeDeviceCapabilities(ctx, responseBody, s.capabilitiesController); err != nil {
			return err
		}
	}
	if projectionObservation != nil {
		bundle, err := envinject.DecodeProjectionResponse(responseBody, "environment_bundle")
		if err != nil {
			return err
		}
		if bundle != nil {
			if err := s.projection.Apply(ctx, *bundle); err != nil {
				return err
			}
		}
	}

	if environmentObservation != nil {
		bundle, err := envinject.DecodeRuntimeResponse(responseBody)
		if err != nil {
			slog.Warn("runtime environment response could not be applied", "error", err)
		} else if bundle != nil {
			if err := s.environment.Apply(ctx, *bundle); err != nil {
				slog.Warn("runtime environment bundle could not be applied", "error", err)
			}
		}
	}
	if environmentObservationSent && s.onEnvironmentObservation != nil {
		s.onEnvironmentObservation()
	}
	if s.receiptPath == "" {
		return nil
	}
	return writeServerHeartbeatReceipt(s.receiptPath, serverHeartbeatReceipt{Schema: "paperboat.server-heartbeat/v1", WorkerGeneration: s.workerGeneration, ReporterVersion: s.reporterVersion, AcceptedAt: time.Now().UTC()})
}

type lazyRuntimeObservation struct {
	Schema                 string    `json:"schema"`
	BootID                 string    `json:"boot_id"`
	InstallationGeneration uint64    `json:"installation_generation"`
	StartedAt              time.Time `json:"started_at"`
}

func (s *runtimeObservationSender) lazyRuntimeObservation() *lazyRuntimeObservation {
	if s.lazyBootID == "" || s.installationGeneration == 0 || s.lazyStartedAt.IsZero() {
		return nil
	}
	return &lazyRuntimeObservation{Schema: "paperboat.lazy-runtime/v1", BootID: s.lazyBootID, InstallationGeneration: s.installationGeneration, StartedAt: s.lazyStartedAt.UTC()}
}

type runtimeUpdateObservation struct {
	Schema                 string    `json:"schema"`
	State                  string    `json:"state"`
	CurrentVersion         string    `json:"current_version"`
	TargetVersion          string    `json:"target_version,omitempty"`
	Channel                string    `json:"channel"`
	OperationID            string    `json:"operation_id"`
	InstallationGeneration uint64    `json:"installation_generation"`
	WorkerGeneration       uint64    `json:"worker_generation"`
	OSBootID               string    `json:"os_boot_id"`
	RollbackCount          uint64    `json:"rollback_count"`
	ErrorCode              string    `json:"error_code,omitempty"`
	ObservedAt             time.Time `json:"observed_at"`
}

func (s *runtimeObservationSender) updateObservation(now time.Time, availabilityState *availability.Observation) *runtimeUpdateObservation {
	return s.updateObservationFrom(now, availabilityState, nil, nil)
}

func (s *runtimeObservationSender) updateObservationFrom(now time.Time, availabilityState *availability.Observation, updaterState *updated.ControlResponse, updaterErr error) *runtimeUpdateObservation {
	if s.reporterVersion == "" || s.installationGeneration == 0 || s.workerGeneration == 0 || s.osBootID == "" {
		return nil
	}
	state, target, errorCode := "healthy", "", ""
	var rollbackCount uint64
	if updaterErr != nil {
		return &runtimeUpdateObservation{
			Schema: "paperboat.update-observation/v1", State: "failed", CurrentVersion: s.reporterVersion,
			TargetVersion: s.reporterVersion, Channel: "stable", OperationID: "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
			InstallationGeneration: s.installationGeneration, WorkerGeneration: s.workerGeneration, OSBootID: s.osBootID,
			ErrorCode: "updater_unavailable", ObservedAt: now,
		}
	}
	if updaterState != nil {
		currentVersion := s.reporterVersion
		if updaterState.Version != "" {
			currentVersion = updaterState.Version
		}
		if updaterState.Observation.Failure != "" || updaterState.Status != "ok" {
			return &runtimeUpdateObservation{
				Schema: "paperboat.update-observation/v1", State: "failed", CurrentVersion: currentVersion,
				TargetVersion: currentVersion, Channel: "stable", OperationID: "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
				InstallationGeneration: s.installationGeneration, WorkerGeneration: s.workerGeneration, OSBootID: s.osBootID,
				ErrorCode: "update_failed", ObservedAt: now,
			}
		}
		// The updater owns activation and is authoritative for the installed
		// version. The runtime process can remain alive across that activation.
		return &runtimeUpdateObservation{
			Schema: "paperboat.update-observation/v1", State: "healthy", CurrentVersion: currentVersion,
			Channel: "stable", OperationID: "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
			InstallationGeneration: s.installationGeneration, WorkerGeneration: s.workerGeneration, OSBootID: s.osBootID,
			ObservedAt: now,
		}
	}
	if availabilityState != nil {
		if availabilityState.UpdateHealth == "unknown" {
			return nil
		}
		rollbackCount = availabilityState.UpdateRollbacks
		if availabilityState.UpdateHealth == "recovery_required" {
			state, target, errorCode = "failed", s.reporterVersion, "recovery_required"
		}
	}
	channel := "stable"
	return &runtimeUpdateObservation{
		Schema:                 "paperboat.update-observation/v1",
		State:                  state,
		CurrentVersion:         s.reporterVersion,
		TargetVersion:          target,
		Channel:                channel,
		OperationID:            "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
		InstallationGeneration: s.installationGeneration,
		WorkerGeneration:       s.workerGeneration,
		OSBootID:               s.osBootID,
		RollbackCount:          rollbackCount,
		ErrorCode:              errorCode,
		ObservedAt:             now,
	}
}

type runtimeRelayLatencySample struct {
	Region string `json:"region"`
	RTTMS  int64  `json:"rtt_ms"`
}

type runtimeRelayLatencyObservation struct {
	Generation         uint64                      `json:"generation"`
	ObservedAt         time.Time                   `json:"observed_at"`
	Samples            []runtimeRelayLatencySample `json:"samples"`
	RelaySuccessRegion string                      `json:"relay_success_region,omitempty"`
	RelaySuccessAt     time.Time                   `json:"relay_success_at,omitempty"`
}

func (s *runtimeObservationSender) nextRelayLatency(now time.Time) *runtimeRelayLatencyObservation {
	if s.relayLatency == nil || s.workerGeneration == 0 {
		return nil
	}
	vector := s.relayLatency.Vector(now)
	if len(vector.Samples) == 0 || len(vector.Samples) > relayselection.MaximumRegions {
		return nil
	}
	s.relayLatencyMu.Lock()
	s.relayLatencyGen++
	generation := s.relayLatencyGen
	s.relayLatencyMu.Unlock()
	result := &runtimeRelayLatencyObservation{Generation: generation, ObservedAt: now, Samples: make([]runtimeRelayLatencySample, 0, len(vector.Samples))}
	if s.relaySuccess != nil {
		region, observedAt := s.relaySuccess.Success()
		if region != "" && !observedAt.IsZero() && !observedAt.After(now) && now.Sub(observedAt) <= 30*time.Second {
			result.RelaySuccessRegion, result.RelaySuccessAt = region, observedAt
		}
	}
	for _, sample := range vector.Samples {
		milliseconds := (sample.RTT + time.Millisecond - 1) / time.Millisecond
		if sample.Region == "" || milliseconds < 1 || milliseconds > 60_000 {
			return nil
		}
		result.Samples = append(result.Samples, runtimeRelayLatencySample{Region: sample.Region, RTTMS: int64(milliseconds)})
	}
	return result
}

type runtimeDiagnosticsObservation struct {
	Capabilities        []string  `json:"capabilities"`
	WorkerGeneration    uint64    `json:"worker_generation"`
	OSBootID            string    `json:"os_boot_id"`
	ConnectorState      string    `json:"connector_state"`
	ConnectorGeneration uint64    `json:"connector_generation"`
	WorkerServiceScope  string    `json:"worker_service_scope"`
	ObservedAt          time.Time `json:"observed_at"`
}

func (s *runtimeObservationSender) runtimeDiagnostics(observedAt time.Time, environmentEnabled bool) *runtimeDiagnosticsObservation {
	if s.workerGeneration < 1 || s.osBootID == "" || s.connector == nil {
		return nil
	}
	status := s.connector.Status()
	state := "unavailable"
	if status.Connected {
		state = "ready"
	} else if status.Stopping {
		state = "degraded"
	}
	capabilities := append([]string(nil), s.capabilities...)
	if len(capabilities) == 0 {
		capabilities = []string{"file_receive", "preview_launch", "terminal_host", "codex_host", "session_host", "keep_awake"}
	}
	if environmentEnabled && !slices.Contains(capabilities, "environment_injection") {
		capabilities = append(capabilities, "environment_injection")
	}
	return &runtimeDiagnosticsObservation{Capabilities: capabilities, WorkerGeneration: s.workerGeneration, OSBootID: s.osBootID, ConnectorState: state, ConnectorGeneration: status.Generation, WorkerServiceScope: s.serviceScope, ObservedAt: observedAt}
}

type serverHeartbeatReceipt struct {
	Schema           string    `json:"schema"`
	WorkerGeneration uint64    `json:"worker_generation"`
	ReporterVersion  string    `json:"reporter_version"`
	AcceptedAt       time.Time `json:"accepted_at"`
}

func writeServerHeartbeatReceipt(path string, receipt serverHeartbeatReceipt) error {
	if !filepath.IsAbs(path) || receipt.WorkerGeneration < 1 || receipt.ReporterVersion == "" || receipt.AcceptedAt.IsZero() {
		return ErrProductionInvalid
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrProductionInvalid
	}
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}

func availabilityObservation(source interface {
	Observation() *availability.Observation
}) *availability.Observation {
	if source == nil {
		return nil
	}
	return source.Observation()
}

type jwksRefreshService struct {
	cache    *auth.JWKSCache
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

func (s *jwksRefreshService) Start(context.Context) error {
	if s.cache == nil || s.interval <= 0 || s.cancel != nil {
		return ErrProductionInvalid
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				attemptCtx, attemptCancel := context.WithTimeout(ctx, 10*time.Second)
				_ = s.cache.Refresh(attemptCtx)
				attemptCancel()
				timer.Reset(s.interval)
			}
		}
	}()
	return nil
}

func (s *jwksRefreshService) Shutdown(ctx context.Context) error {
	if s.cancel == nil {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
		s.cancel, s.done = nil, nil
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validatedControlURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, ErrProductionInvalid
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}
func productionTransport(caPath string, environ func(string) string) (http.RoundTripper, error) {
	if environ == nil {
		return nil, ErrProductionInvalid
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
	if caPath != "" {
		if !filepath.IsAbs(caPath) {
			return nil, ErrProductionInvalid
		}
		info, err := os.Lstat(caPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || info.Size() < 1 || info.Size() > 1<<20 {
			return nil, ErrProductionInvalid
		}
		encoded, err := os.ReadFile(caPath)
		if err != nil {
			return nil, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(encoded) {
			return nil, ErrProductionInvalid
		}
		tlsConfig.RootCAs = roots
	}
	transportConfig := httptransport.DevelopmentConfig()
	transportConfig.TLSConfig = tlsConfig
	administrator := httptransport.ProxySnapshot{
		HTTPProxy:  strings.TrimSpace(environ("PAPERBOAT_HTTP_PROXY")),
		HTTPSProxy: strings.TrimSpace(environ("PAPERBOAT_HTTPS_PROXY")),
		NoProxy:    strings.TrimSpace(environ("PAPERBOAT_NO_PROXY")),
		Generation: 1,
	}
	if err := httptransport.ValidateProxySnapshot(administrator); err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	transportConfig.ProxySource = httptransport.PriorityProxySource{
		Administrator: httptransport.StaticProxySource{Value: administrator},
		Environment:   httptransport.EnvironmentProxySource{},
		System:        httptransport.NativeSystemProxySource{},
	}
	return httptransport.New(transportConfig)
}
func valueOrRuntime(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func durationRuntime(value string, fallback time.Duration) time.Duration {
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value + "s")
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
func safeProductionEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		if !(r >= 'A' && r <= 'Z' || r == '_' || index > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
