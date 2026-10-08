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
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"io"
	"log/slog"
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
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/availability"
	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorapi"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorauth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machinecontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/observability"
	peeridentityenrollment "github.com/pinksaucepasta/paperboat/internal/hostruntime/peeridentity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtimeport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
	"github.com/pinksaucepasta/paperboat/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

var (
	ErrProductionInvalid          = errors.New("invalid production host configuration")
	ErrManagedSSHUnavailable      = errors.New("managed SSH host authority is unavailable")
	errEnvironmentEndpointPending = errors.New("environment endpoint enrollment is pending")
)

type productionClock struct{}

func (productionClock) Now() time.Time { return time.Now().UTC() }

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

func newProductionHost(ctx context.Context, version string, environ func(string) string, tunnelProvider ProductionTunnelAssemblyProvider, ownerDependencies ...HostDependencies) (*Host, error) {
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
	lazyBoot, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	lazyBootID := "process_" + lazyBoot.String()
	lazyStartedAt := time.Now().UTC()
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
		id, err := uuid.NewRandom()
		if err != nil {
			return "", err
		}
		return "operation_" + id.String(), nil
	}
	renewingTokens, err := enrollment.NewRenewingTokenSource(enrollment.RenewingTokenConfig{ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, Transport: transport, RenewBefore: 10 * time.Minute, Timeout: 15 * time.Second, Clock: func() time.Time { return time.Now().UTC() }, OperationID: operationID, Metrics: metrics})
	if err != nil {
		return nil, err
	}
	if _, loadErr := enrollment.LoadRuntimeIdentityForRenewal(runtimeConfig.StateRoot, time.Now().UTC()); loadErr != nil {
		enrollmentClient, clientErr := enrollment.NewClient(transport, 15*time.Second)
		if clientErr != nil {
			return nil, clientErr
		}
		enrollmentConfig := enrollment.Config{
			ControlURL: controlURL.String(), ControlCAFile: environ("PAPERBOAT_CONTROL_CA_FILE"),
			StateRoot: runtimeConfig.StateRoot,
		}
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
		if err != nil {
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
	identityStore, openErr := runtimeidentity.Open(runtimeidentity.Config{StateRoot: runtimeConfig.StateRoot})
	if openErr != nil {
		return nil, openErr
	}
	machineRegistration, registrationErr := identityStore.Registration()
	if registrationErr != nil || machineRegistration.MachineID != machineID || !filepath.IsAbs(machineRegistration.InboxPath) || filepath.Clean(machineRegistration.InboxPath) != machineRegistration.InboxPath {
		return nil, errors.Join(ErrProductionInvalid, registrationErr)
	}
	inboxPath := machineRegistration.InboxPath
	// Host enrollment uses the renewable helper token and proof for managed SSH.
	managedSSHIdentity := managedSSHIdentitySource(renewingMachineIdentity{tokens: renewingTokens, proofs: enrollment.ProofSource{StateRoot: runtimeConfig.StateRoot}})
	networkFingerprintSecret, err := identityStore.NetworkFingerprintSecret()
	if err != nil {
		return nil, err
	}
	defer clear(networkFingerprintSecret)
	peerEnrollment, err := peeridentityenrollment.New(peeridentityenrollment.Config{ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, Transport: transport, Timeout: 15 * time.Second}, managedSSHIdentity)
	if err != nil {
		return nil, err
	}
	if err := allowPendingPeerEnrollment(ctx, peerEnrollment); err != nil {
		return nil, err
	}
	var managedEnvironment envinject.EnvironmentSource
	var runtimeLayers *layerEnvironmentService
	var environmentBootstrap Service
	if environmentInjectionEligible(machineRegistration) {
		runtimeLayers = newLayerEnvironmentService(runtimeConfig.StateRoot, controlURL, transport, machineRegistration, managedSSHIdentity)
		managedEnvironment, environmentBootstrap = runtimeLayers, runtimeLayers
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
	credentialConfig := CredentialAuthConfig{InstallationGeneration: machineRegistration.InstallationGeneration, Issuer: issuer, EnvironmentID: identity.EnvironmentID, MachineID: machineID, HelperID: identity.HelperID, Verifier: verifier, Revocations: revocations}
	authorizer, err := NewCredentialAuthorizer(credentialConfig)
	if err != nil {
		return nil, err
	}
	browserTerminalAuthorizer, err := NewBrowserTerminalCredentialAuthorizer(credentialConfig)
	if err != nil {
		return nil, err
	}
	// No transfer is admitted until the authenticated heartbeat supplies policy.
	transferPolicy := &filetransfer.PolicyStore{}
	capabilityController := newMachineCapabilityController(nil)
	networkHandler, err := newNetworkChangeHandler(metrics)
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
	connectorService := &dedicatedConnectorService{networkChanges: networkChanges}
	var runtimeObservation *runtimeObservationService
	resolver, resolverErr := availability.NewResolver(controlURL.ResolveReference(&url.URL{Path: "/v1/helper-runtime-policies/resolve"}).String(), renewingTokens, enrollment.ProofSource{StateRoot: runtimeConfig.StateRoot}, operationID, &http.Client{Transport: transport, Timeout: 10 * time.Second})
	if resolverErr != nil {
		return nil, resolverErr
	}
	hostClient, hostErr := newProductionAvailabilityHostClient(5 * time.Second)
	if hostErr != nil {
		return nil, hostErr
	}
	availabilityService, err := availability.NewService(resolver, hostClient, runtimeConfig.Limits.HeartbeatInterval, metrics)
	if err != nil {
		return nil, err
	}
	var machineServiceObserver *runtimeObservationSender
	{
		runtimeEndpoint := controlURL.ResolveReference(&url.URL{Path: "/v1/runtime-observations"}).String()
		scope := environ("PAPERBOAT_RUNTIME_SERVICE_SCOPE")
		if scope != "system" && scope != "user" {
			scope = "unknown"
		}
		capabilities := []string{"file_receive", "preview_launch", "terminal_host", "session_host", "keep_awake"}
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
		sender := &runtimeObservationSender{endpoint: runtimeEndpoint, tokens: observationTokens, proofs: enrollment.ProofSource{StateRoot: runtimeConfig.StateRoot}, operationID: operationID, environmentID: identity.EnvironmentID, machineID: machineID, reporterVersion: version, client: &http.Client{Transport: errorreport.TransportOperation(transport, controlURL.String(), "runtime_observation"), Timeout: 10 * time.Second, CheckRedirect: rejectRuntimePolicyRedirect}, availability: availabilityService, receiptPath: filepath.Join(runtimeConfig.StateRoot, "runtime", "server-heartbeat.json"), installationGeneration: uint64(machineRegistration.InstallationGeneration), workerGeneration: bootState.Generation, osBootID: bootState.OSBootID, lazyBootID: lazyBootID, lazyStartedAt: lazyStartedAt, serviceScope: scope, connector: connectorService, transferPolicy: transferPolicy, capabilitiesController: capabilityController, capabilities: capabilities}
		machineServiceObserver = sender
		if runtimeLayers != nil {
			sender.layers = runtimeLayers
		}
		updaterClient, updaterErr := newProductionUpdaterClient()
		if updaterErr != nil {
			return nil, updaterErr
		}
		sender.updater = updaterClient
		runtimeObservation = &runtimeObservationService{sender: sender, interval: 10 * time.Second, timeout: 10 * time.Second}
	}
	workspaceRoot := environ("PAPERBOAT_WORKSPACE_ROOT")
	if strings.TrimSpace(workspaceRoot) == "" {
		workspaceRoot, err = os.UserHomeDir()
		if err != nil {
			return nil, errors.Join(ErrProductionInvalid, errors.New("resolve machine home workspace"), err)
		}
	}
	agentShell, err := validatedMachineShell(environ("PAPERBOAT_SHELL"))
	if err != nil {
		return nil, err
	}
	agentEnvironment, err := process.BaseEnvironment(agentShell)
	if err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	shutdownTimeout := 30 * time.Second
	if err := validateMachineWorkspace(workspaceRoot); err != nil {
		return nil, err
	}
	listen := valueOrRuntime(environ("PAPERBOAT_RUNTIME_LISTEN_ADDRESS"), runtimeport.Primary)
	localControlToken, err := writeLocalControlToken(runtimeConfig.StateRoot)
	if err != nil {
		return nil, err
	}
	inspectorService, inspectorStore, inspectorRegistry, err := newInspectorService(controlURL.String(), runtimeConfig.StateRoot, transport)
	if err != nil {
		return nil, err
	}
	if err := writeWorkerLocal(runtimeConfig.StateRoot, listen); err != nil {
		return nil, err
	}
	runtimeService := serviceGroup{availabilityService, runtimeObservation}
	previewAssembly, err := newProductionPreviewAssembly(productionPreviewAssemblyConfig{
		ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, MachineID: machineID, InstallationGeneration: machineRegistration.InstallationGeneration, BootID: lazyBootID,
		LocalControlToken: localControlToken, Transport: transport, RunContext: ctx,
		InspectorStore: inspectorStore, InspectorRegistry: inspectorRegistry, InspectorHTTP: inspectorService,
	})
	if err != nil {
		return nil, err
	}
	managedSSHHost, managedSSHService, err := newProductionManagedSSH(controlURL.String(), transport, machineRegistration, managedSSHIdentity, uint64(bootState.Generation), capabilityController)
	if err != nil {
		return nil, err
	}
	browserCompareAuthorizer, err := NewBrowserConfigCompareCredentialAuthorizer(credentialConfig)
	if err != nil {
		return nil, err
	}
	compareIdentity, err := machinecontrol.NewSource(machinecontrol.Config{ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, Transport: transport})
	if err != nil {
		return nil, err
	}
	comparisonHome, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	comparisonHosts := []string{"github.com"}
	if raw := strings.TrimSpace(environ("PAPERBOAT_CONFIG_REPOSITORY_HOSTS")); raw != "" {
		comparisonHosts = strings.Split(raw, ",")
	}
	comparison, err := productionConfigComparison(productionConfigSyncConfig{ControlURL: controlURL.String(), ControlHost: controlURL.Hostname(), RepositoryHosts: comparisonHosts, HomeRoot: filepath.Clean(comparisonHome), StateRoot: runtimeConfig.StateRoot, Identities: compareIdentity, Proofs: compareIdentity, OperationID: operationID, Transport: transport})
	if err != nil {
		return nil, err
	}
	dependencies := HostDependencies{ConfigCompare: comparison, BrowserConfigCompareAuthorizer: browserCompareAuthorizer, Authorizer: authorizer, BrowserTerminalAuthorizer: browserTerminalAuthorizer, BrowserTerminalIdentity: func(identityCtx context.Context) (server.BrowserTerminalIdentity, error) {
		if err := identityCtx.Err(); err != nil {
			return server.BrowserTerminalIdentity{}, err
		}
		return productionBrowserTerminalIdentity(runtimeConfig.StateRoot, machineID, uint64(machineRegistration.InstallationGeneration))
	}, AuthorizationService: authorizationRefresh, Connector: connectorService, PreviewDispatcher: previewAssembly, PreviewRecovery: previewAssembly, PreviewOwnerSessions: previewAssembly.OwnerSessionLeases(), RuntimeObservationService: runtimeService, RecordTerminalJoin: runtimeObservation.sender.RecordTerminalJoin, ManagedEnvironment: managedEnvironment, Metrics: metrics, LocalControlToken: localControlToken, Inspector: inspectorService, ManagedSSH: managedSSHHost, ManagedSSHService: managedSSHService, Capabilities: capabilityController}
	{
		attachment, attachErr := runtimeattachment.New(runtimeattachment.Config{ControlURL: controlURL.String(), StateRoot: runtimeConfig.StateRoot, Transport: transport, MachineID: machineID, WorkerGeneration: bootState.Generation, InstallationGeneration: uint64(machineRegistration.InstallationGeneration), ListenAddress: listen})
		if attachErr != nil {
			return nil, attachErr
		}
		dependencies.RuntimeAttachmentService = attachment
	}
	nativePrivateValidators := []productionNativePrivateValidator{localNativePrivateValidator(previewAssembly.dispatcher)}
	if machineServiceObserver != nil {
		nativePrivateValidators = append(nativePrivateValidators, machineServiceObserver.validateMachineService)
	}
	if tunnelProvider == nil {
		tunnelEnrollment, enrollmentErr := newProductionTunnelEnrollmentService(controlURL.String(), runtimeConfig.StateRoot, machineID, localControlToken, transport, inspectorStore, inspectorRegistry, inspectorService)
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
		nativePrivateValidators = append(nativePrivateValidators, localNativePrivateValidator(tunnelAssembly.Manager.Manager))
		updateGate, gateErr := tunnelmanager.NewUpdateGate(tunnelmanager.UpdateGateConfig{MachineID: machineID, Manager: tunnelAssembly.Manager.Manager, StatePath: filepath.Join(runtimeConfig.StateRoot, "updates", "deployment-gate.json")})
		if gateErr != nil {
			return nil, errors.Join(ErrProductionInvalid, gateErr)
		}
		dependencies.UpdateGate = updateGate
		networkHandler.SetCanonical(tunnelAssembly)
	}
	if managedSSHIdentity != nil {
		dependencies.NativePeerFactory = func(serve func(net.Conn) error, transferHandler http.Handler) (Service, error) {
			return newProductionNativePeerService(productionNativePeerConfig{controlURL: controlURL.String(), issuer: issuer, stateRoot: runtimeConfig.StateRoot, machineID: machineID, generation: uint64(machineRegistration.InstallationGeneration), transport: transport, identity: managedSSHIdentity, keys: cache, authorizer: authorizer, serve: serve, transfer: transferHandler, ssh: managedSSHHost, privateCurrent: productionNativeCurrent(nativePrivateValidators...), privateDial: productionNativePrivateDial, inspector: http.HandlerFunc(inspectorService.ServeAuthenticatedHTTP), inspectorStore: inspectorStore, usage: dependencies.Bandwidth})
		}
	}
	if managedSSHIdentity != nil {
		usageControl := clientapi.New(controlURL.String(), clientconfig.Credential{}, &http.Client{Transport: transport, Timeout: 15 * time.Second})
		usageControl.SetMachineAuth(managedSSHIdentity)
		observeBandwidth := newBandwidthObservation(ctx)
		dependencies.Bandwidth, err = bandwidth.Open(filepath.Join(runtimeConfig.StateRoot, "bandwidth-usage.json"), usageControl, observeBandwidth)
		if err != nil {
			return nil, err
		}
	}
	if len(ownerDependencies) != 0 {
		dependencies.Sessions = ownerDependencies[0].Sessions
		dependencies.Executions = ownerDependencies[0].Executions
		dependencies.ReuseAgentToken = true
	}
	result, err := NewHost(ctx, HostConfig{Runtime: runtimeConfig, ListenAddress: listen, WorkspaceRoot: workspaceRoot, ShellPath: agentShell, AgentEnvironment: agentEnvironment, EnvironmentID: identity.EnvironmentID, MachineID: machineID, InboxPath: inboxPath, ShutdownTimeout: shutdownTimeout, RecoveryExitSignal: recoveryExitSignal, FileTransferPolicy: transferPolicy}, dependencies)
	if err == nil {
		capabilityController.SetReconciler(result.reconcileMachineCapabilities)
	}
	return result, err
}

func environmentInjectionEligible(registration runtimeidentity.Registration) bool {
	return registration.MachineID != "" && registration.InstallationGeneration > 0
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

type renewingMachineIdentity struct {
	tokens managedSSHIdentityTokens
	proofs managedSSHIdentityProofs
}

func (s renewingMachineIdentity) Token(ctx context.Context) (string, error) {
	return s.tokens.Token(ctx)
}

func (s renewingMachineIdentity) Proof(ctx context.Context, operationID, method, path string, body []byte) ([]byte, error) {
	return s.proofs.Proof(ctx, operationID, method, path, body)
}

type managedSSHControlClient interface {
	ObserveManagedSSHHostKeys(context.Context, string, string, string, uint64, uint64, []string, []byte) (clientapi.ManagedSSHHostKeySet, error)
	ManagedSSHAuthorizedKeys(context.Context, string, string, uint64, []byte) (clientapi.ManagedSSHAuthorizedKeys, error)
}

func initializeProductionManagedSSHUnix(ctx context.Context, host *managedssh.Host, controlURL string, transport http.RoundTripper, registration runtimeidentity.Registration, identitySource managedSSHIdentitySource, observationGeneration uint64) (Service, error) {
	if registration.MachineID == "" || registration.InstallationGeneration < 1 || registration.SSHPort == 0 || registration.SSHUser == "" || identitySource == nil {
		return nil, nil
	}
	var err error
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err = host.ReconcileTarget(probeCtx, uint64(registration.InstallationGeneration), registration.SSHPort)
	cancel()
	if err != nil {
		return nil, errors.Join(ErrManagedSSHUnavailable, err)
	}
	paths := existingSSHHostPublicKeyPathsUnix()
	if len(paths) == 0 {
		return nil, errors.Join(ErrManagedSSHUnavailable, errors.New("no SSH host public key is published"))
	}
	inventory, err := managedssh.ReadHostPublicKeys(paths, 0)
	if err != nil {
		return nil, errors.Join(ErrManagedSSHUnavailable, err)
	}
	if observationGeneration == 0 {
		return nil, errors.New("managed SSH observation generation is unavailable")
	}
	publicKeys := make([]string, len(inventory.Keys))
	for index := range inventory.Keys {
		publicKeys[index] = inventory.Keys[index].PublicKey
	}
	client := clientapi.New(controlURL, clientconfig.Credential{}, &http.Client{Transport: transport, Timeout: 15 * time.Second})
	account, err := user.Lookup(registration.SSHUser)
	if err != nil || !filepath.IsAbs(account.HomeDir) {
		return nil, errors.Join(ErrManagedSSHUnavailable, errors.New("managed SSH operating-system user is unavailable"), err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, errors.Join(ErrManagedSSHUnavailable, errors.New("managed SSH operating-system user identifier is invalid"), err)
	}
	reconciler := &managedSSHKeyReconciler{
		client: client, identity: identitySource, registration: registration,
		workerGeneration: observationGeneration, publicKeys: publicKeys,
		home: account.HomeDir, ownerUID: uint32(uid), interval: 30 * time.Second, timeout: 10 * time.Second,
	}
	return reconciler, nil
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

func reconcileManagedSSHAuthorityWithFingerprint(ctx context.Context, client managedSSHControlClient, identitySource managedSSHIdentitySource, registration runtimeidentity.Registration, observationGeneration uint64, fingerprint [32]byte, publicKeys []string) (clientapi.ManagedSSHAuthorizedKeys, bool, error) {
	observeOperationID, keyOperationID := managedSSHInitialOperationIDs(registration, observationGeneration, fingerprint)
	return reconcileManagedSSHAuthorityWithOperations(ctx, client, identitySource, registration, observationGeneration, publicKeys,
		observeOperationID, keyOperationID)
}

func reconcileManagedSSHAuthorityWithOperations(ctx context.Context, client managedSSHControlClient, identitySource managedSSHIdentitySource, registration runtimeidentity.Registration, observationGeneration uint64, publicKeys []string, observeOperationID, keyOperationID string) (clientapi.ManagedSSHAuthorizedKeys, bool, error) {
	if ctx == nil || client == nil || identitySource == nil || registration.MachineID == "" || registration.InstallationGeneration < 1 || observationGeneration == 0 || len(publicKeys) == 0 {
		return clientapi.ManagedSSHAuthorizedKeys{}, false, ErrProductionInvalid
	}
	body, err := json.Marshal(map[string]any{"observation_generation": observationGeneration, "public_keys": publicKeys})
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
	set, err := client.ObserveManagedSSHHostKeys(ctx, registration.MachineID, identityCredential, observeOperationID, uint64(registration.InstallationGeneration), observationGeneration, publicKeys, proof)
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

func productionBrowserTerminalIdentity(stateRoot, machineID string, generation uint64) (server.BrowserTerminalIdentity, error) {
	endpoint, err := runtimeEnvironmentEndpoint(stateRoot)
	if err != nil || endpoint.Generation != generation || len(endpoint.Certificate) == 0 {
		return server.BrowserTerminalIdentity{}, errors.Join(errEnvironmentEndpointPending, err)
	}
	rootFingerprint, err := endpointidentity.RootFingerprint(endpoint.RootPublicKey)
	if err != nil || endpoint.RootKeyID != "aek_"+rootFingerprint {
		return server.BrowserTerminalIdentity{}, errors.Join(ErrProductionInvalid, err)
	}
	certificate, err := endpointidentity.Verify(endpoint.Certificate, endpoint.RootPublicKey, endpointidentity.Expected{Role: endpointidentity.RoleMachine, EndpointID: machineID, Generation: generation}, time.Now().UTC())
	if err != nil {
		return server.BrowserTerminalIdentity{}, errors.Join(ErrProductionInvalid, err)
	}
	now := time.Now().UTC()
	lifetime := certificate.Claims.ExpiresAt.Sub(now)
	if lifetime > 24*time.Hour {
		lifetime = 24 * time.Hour
	}
	leaf, err := endpointidentity.NewTLSCertificate(certificate, endpoint.RootPublicKey, endpoint.QUICPrivateKey, now, lifetime)
	if err != nil {
		return server.BrowserTerminalIdentity{}, errors.Join(ErrProductionInvalid, err)
	}
	return server.BrowserTerminalIdentity{Certificate: append([]byte(nil), endpoint.Certificate...), RootKeyID: endpoint.RootKeyID, TLSCertificate: leaf}, nil
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

// newInspectorService creates the daemon's one shared inspector capture
// store, replay registry and loopback HTTP service. Captures stay in memory;
// a restart deletes them. Every operation re-verifies the caller's grant
// against live server authority over the host's machine identity; there is no
// local-owner substitution.
func newInspectorService(controlURL, stateRoot string, transport http.RoundTripper) (*inspectorapi.Service, *inspector.Store, *inspector.Registry, error) {
	source, err := machinecontrol.NewSource(machinecontrol.Config{ControlURL: controlURL, StateRoot: stateRoot, Transport: transport})
	if err != nil {
		return nil, nil, nil, err
	}
	authorize, err := inspectorauth.Config{BaseURL: controlURL, Source: source}.AuthorizeFunc()
	if err != nil {
		return nil, nil, nil, err
	}
	store := inspector.NewStore()
	registry := inspector.NewRegistry()
	service, err := inspectorapi.New(inspectorapi.Config{Store: store, Registry: registry, Authorize: authorize})
	if err != nil {
		return nil, nil, nil, err
	}
	return service, store, registry, nil
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

func validatedMachineShellUnix(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/bin/sh"
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.Join(ErrProductionInvalid, errors.New("machine shell must be an absolute canonical path"))
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.Join(ErrProductionInvalid, errors.New("machine shell must resolve to an absolute canonical path"))
	}
	resolved := path
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err = filepath.EvalSymlinks(path)
	}
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", errors.Join(ErrProductionInvalid, errors.New("machine shell must resolve to an absolute canonical path"))
	}
	info, err = os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.Join(ErrProductionInvalid, errors.New("machine shell must be an executable regular file"))
	}
	return resolved, nil
}

func validateMachineWorkspaceUnix(root string) error {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.Join(ErrProductionInvalid, errors.New("machine workspace must be an absolute canonical path"))
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.Join(ErrProductionInvalid, errors.New("machine workspace must be an existing non-symlink directory"))
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return errors.Join(ErrProductionInvalid, errors.New("machine workspace symlink resolution is not permitted"))
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
	if err := ctx.Err(); err != nil {
		return err
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
		reportRuntimeObservationAttempt(initial, err)
	}
	if err := ctx.Err(); err != nil {
		return err
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
	runCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
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
				reportRuntimeObservationAttempt(sendCtx, err)
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
	machineServicesDiscover                             func(context.Context) ([]machineservices.Service, error)
	machineServicesMu                                   sync.Mutex
	machineServicesPolicy                               clientapi.MachineServicesPolicy
	machineServicesPorts                                []clientapi.MachineServicePort
	machineServicesGeneration                           uint64
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
	layers                 interface{ FlushLayerObservations(context.Context) error }
	receiptPath            string
	installationGeneration uint64
	workerGeneration       uint64
	osBootID               string
	lazyBootID             string
	lazyStartedAt          time.Time
	serviceScope           string
	connector              interface{ Status() connector.Status }
	transferPolicy         *filetransfer.PolicyStore
	capabilities           []string
	capabilitiesController *machineCapabilityController
	environmentFaultMu     sync.Mutex
	environmentFaults      [4]errorreport.Fault
}

func (s *runtimeObservationSender) Send(ctx context.Context) error {
	if s.layers != nil {
		s.observeEnvironmentFailure(ctx, 0, s.layers.FlushLayerObservations(ctx))
	}
	now := time.Now().UTC()
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
		MachineServices     *clientapi.MachineServicesSnapshot `json:"machine_services,omitempty"`
		LazyRuntime         *lazyRuntimeObservation            `json:"lazy_runtime,omitempty"`
		EnvironmentID       string                             `json:"environment_id"`
		ResourceID          string                             `json:"resource_id"`
		ReporterVersion     string                             `json:"reporter_version"`
		SampledAt           time.Time                          `json:"sampled_at"`
		Availability        *availability.Observation          `json:"availability,omitempty"`
		RuntimeDiagnostics  *runtimeDiagnosticsObservation     `json:"runtime_diagnostics,omitempty"`
		Update              *runtimeUpdateObservation          `json:"update,omitempty"`
		MachineCapabilities *machineCapabilitiesObservation    `json:"machine_capabilities,omitempty"`
	}{
		MachineServices:     s.machineServicesObservation(ctx),
		LazyRuntime:         s.lazyRuntimeObservation(),
		EnvironmentID:       s.environmentID,
		ResourceID:          s.machineID,
		ReporterVersion:     s.reporterVersion,
		SampledAt:           now,
		Availability:        availabilityState,
		RuntimeDiagnostics:  s.runtimeDiagnostics(now, s.layers != nil),
		Update:              s.updateObservationFrom(now, availabilityState, updaterState, updaterErr),
		MachineCapabilities: s.capabilitiesController.Observation(now),
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
		return runtimeObservationFailure{stage: "peer_authority", cause: err}
	}
	operationID, err := s.operationID()
	if err != nil {
		return err
	}
	proof, err := s.proofs.Proof(ctx, operationID, http.MethodPost, "/v1/runtime-observations", body)
	if err != nil {
		return runtimeObservationFailure{stage: "peer_authority", cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return runtimeObservationFailure{stage: "control_request", cause: err}
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	defer func() {
		for index := range responseBody {
			responseBody[index] = 0
		}
	}()
	if readErr != nil {
		return runtimeObservationFailure{stage: "delivery", cause: readErr}
	}
	if len(responseBody) > 8<<20 {
		return errors.New("runtime observation response is invalid")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.applyMachineServicesPolicy(nil)
		return runtimeObservationFailure{stage: "control_request", cause: errorreport.HTTPStatusFailure(response)}
	}
	s.applyMachineServicesPolicy(responseBody)
	if s.transferPolicy != nil {
		if err := applyRuntimeTransferPolicy(responseBody, s.transferPolicy); err != nil {
			return err
		}
	}
	if s.capabilitiesController != nil {
		if err := applyRuntimeMachineCapabilities(ctx, responseBody, s.capabilitiesController); err != nil {
			return err
		}
	}
	if s.receiptPath == "" {
		return nil
	}
	return writeServerHeartbeatReceipt(s.receiptPath, serverHeartbeatReceipt{Schema: "paperboat.server-heartbeat/v1", WorkerGeneration: s.workerGeneration, ReporterVersion: s.reporterVersion, AcceptedAt: time.Now().UTC()})
}

type runtimeObservationFailure struct {
	stage string
	cause error
}

func (runtimeObservationFailure) Error() string                   { return "runtime observation failed" }
func (failure runtimeObservationFailure) Unwrap() error           { return failure.cause }
func (failure runtimeObservationFailure) DiagnosticStage() string { return failure.stage }
func (runtimeObservationFailure) DiagnosticCode() string          { return "control_request_failed" }

func reportRuntimeObservationAttempt(ctx context.Context, err error) {
	if errorreport.HTTPAttemptObserved(err) {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "runtime_observation", "reconciliation", "control_request_failed", err)
}

// Four producer-owned phases bound the retained retry classifications. Only a
// changed fault or actual successful recovery produces another local event.
func (s *runtimeObservationSender) observeEnvironmentFailure(ctx context.Context, phase int, err error) {
	stage := "reconciliation"
	if phase < 2 {
		stage = "diagnostic_storage"
	}
	fault := errorreport.ProjectFault(ctx, "paperboat-daemon", "config_sync", stage, "environment_sync_failed", err)
	if fault.Outcome == "canceled" {
		return
	}
	s.environmentFaultMu.Lock()
	previous := s.environmentFaults[phase]
	s.environmentFaults[phase] = fault
	s.environmentFaultMu.Unlock()
	if err == nil {
		if previous.Code != "" {
			errorreport.Current().Lifecycle(ctx, "config", "config_sync", "recovered", "success")
			if local := diagnostics.FromContext(ctx); local != nil {
				_ = local.RecordWithSupportReference(stage, "recovered", "info", supportref.FromContext(ctx), map[string]string{"component": "paperboat-daemon", "operation": "config_sync"})
			}
		}
		return
	}
	if previous.Code == fault.Code && previous.Stage == fault.Stage && previous.Cause == fault.Cause && previous.Errno == fault.Errno && previous.HTTPStatus == fault.HTTPStatus {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "config_sync", stage, "environment_sync_failed", err)
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
	channel := "custom"
	if buildinfo.Distribution == "official" {
		channel = "stable"
	}
	var rollbackCount uint64
	if updaterErr != nil {
		return &runtimeUpdateObservation{
			Schema: "paperboat.update-observation/v1", State: "failed", CurrentVersion: s.reporterVersion,
			TargetVersion: s.reporterVersion, Channel: channel, OperationID: "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
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
				TargetVersion: currentVersion, Channel: channel, OperationID: "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
				InstallationGeneration: s.installationGeneration, WorkerGeneration: s.workerGeneration, OSBootID: s.osBootID,
				ErrorCode: "update_failed", ObservedAt: now,
			}
		}
		// The updater owns activation and is authoritative for the installed
		// version. The runtime process can remain alive across that activation.
		return &runtimeUpdateObservation{
			Schema: "paperboat.update-observation/v1", State: "healthy", CurrentVersion: currentVersion,
			Channel: channel, OperationID: "update-" + strconv.FormatUint(s.workerGeneration, 10) + "-" + strconv.FormatInt(now.UnixNano(), 10),
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
		capabilities = []string{"file_receive", "preview_launch", "terminal_host", "session_host", "keep_awake"}
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
