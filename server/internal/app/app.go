package app

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/backupreport"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat-server/internal/browseringress"
	"github.com/pinksaucepasta/paperboat-server/internal/browserterminal"
	"github.com/pinksaucepasta/paperboat-server/internal/clientupdate"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/diagnosticuploads"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
	"github.com/pinksaucepasta/paperboat-server/internal/favorites"
	pbgithub "github.com/pinksaucepasta/paperboat-server/internal/github"
	"github.com/pinksaucepasta/paperboat-server/internal/httpapi"
	"github.com/pinksaucepasta/paperboat-server/internal/inspectaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/jobmonitor"
	"github.com/pinksaucepasta/paperboat-server/internal/lazyaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat-server/internal/managedssh"
	"github.com/pinksaucepasta/paperboat-server/internal/metering"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/nativeprivateaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat-server/internal/operator"
	"github.com/pinksaucepasta/paperboat-server/internal/operatoractions"
	"github.com/pinksaucepasta/paperboat-server/internal/operatorauth"
	"github.com/pinksaucepasta/paperboat-server/internal/operatorinfra"
	"github.com/pinksaucepasta/paperboat-server/internal/operatorreporting"
	"github.com/pinksaucepasta/paperboat-server/internal/peeridentity"
	"github.com/pinksaucepasta/paperboat-server/internal/peersessions"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat-server/internal/previewdispatch"
	"github.com/pinksaucepasta/paperboat-server/internal/previewdomain"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat-server/internal/previewv1"
	"github.com/pinksaucepasta/paperboat-server/internal/privateaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/releaseauthority"
	"github.com/pinksaucepasta/paperboat-server/internal/resourceusage"
	"github.com/pinksaucepasta/paperboat-server/internal/runtimeattachment"
	"github.com/pinksaucepasta/paperboat-server/internal/selfhost"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
	syncpkg "github.com/pinksaucepasta/paperboat-server/internal/sync"
	"github.com/pinksaucepasta/paperboat-server/internal/teaminbox"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/telemetry"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"time"
)

func New(opts Options) (*App, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	certificateRuntime := opts.CertificateRuntime
	runtimeTransferred := false
	defer func() {
		if !runtimeTransferred && certificateRuntime != nil {
			_ = certificateRuntime.Close()
		}
	}()
	if certificateRuntime != nil && !opts.Config.Certificates.Enabled {
		return nil, fmt.Errorf("%w: injected runtime requires managed certificates to be enabled", tunnelv1.ErrCertificateRuntimeUnavailable)
	}
	if certificateRuntime != nil && (opts.CertificateWorker != nil || opts.CertificateDistribution != nil) {
		return nil, fmt.Errorf("certificate runtime cannot be combined with legacy certificate options")
	}
	store, err := db.Open(opts.Config.Database)
	if err != nil {
		return nil, err
	}
	if certificateRuntime == nil && opts.Config.Certificates.Enabled {
		certificateRuntime, err = newCertificateRuntime(context.Background(), store, opts.Config)
		if err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	if certificateRuntime != nil {
		opts.CertificateRuntime = certificateRuntime
		opts.CertificateWorker = certificateRuntime.Worker
		opts.CertificateDistribution = certificateRuntime.Distribution.Handler()
	}
	auditWriter := audit.NewWriter(store)
	previewTunnelStore, err := previewtunnelstore.New(store)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview tunnel store: %w", err)
	}
	previewDomainRepository, err := previewdomain.NewSQLRepository(store, previewdomain.Config{})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview domain repository: %w", err)
	}
	if err := previewTunnelStore.ConfigurePreviewDomains(previewDomainBatchCreatorAdapter{repository: previewDomainRepository}); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure atomic preview domains: %w", err)
	}
	previewAttachmentProduction, err := previewattachment.NewProduction(store)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview carrier attachment: %w", err)
	}
	cursorKey := sha256.Sum256([]byte("paperboat.preview-tunnel.cursor\x00" + opts.Config.Secrets.EncryptionKey))
	previewDomainService, err := previewdomain.NewService(previewDomainRepository, previewdomain.Config{
		CursorKey:     cursorKey[:],
		ChallengeZone: opts.Config.Certificates.ChallengeZone,
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview domain service: %w", err)
	}
	previewTunnelAPI, err := previewtunnelapi.NewService(previewTunnelStore, cursorKey[:])
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview tunnel API: %w", err)
	}
	previewLeaseService, err := previewv1.NewService(previewTunnelStore, previewv1.Config{
		EndpointDomain:      opts.Config.Preview.BaseDomain,
		CursorKey:           cursorKey[:],
		AttachmentReadiness: previewAttachmentProduction.Service,
		PreviewDomains:      previewDomainRepository,
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview lease service: %w", err)
	}
	lazyPolicyService, err := lazyaccess.NewService(store, opts.Config.Preview.BaseDomain)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure lazy policy service: %w", err)
	}
	tunnelRepository, err := tunnelv1.NewRepository(store)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure tunnel repository: %w", err)
	}
	tunnelEndpointBuilder, err := tunnelv1.NewEndpointBuilder("https://" + opts.Config.Tunnel.BaseDomain)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure tunnel endpoint builder: %w", err)
	}
	tunnelService, err := tunnelv1.NewService(tunnelRepository, tunnelv1.Config{
		EndpointBuilder:  tunnelEndpointBuilder,
		CursorKey:        cursorKey[:],
		PublicTCPPortMin: int32(opts.Config.Tunnel.PublicTCPPortMin),
		PublicTCPPortMax: int32(opts.Config.Tunnel.PublicTCPPortMax),
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure tunnel service: %w", err)
	}
	tunnelResourceService, err := tunnelv1.NewResourceService(tunnelRepository, tunnelv1.ResourceConfig{
		CursorKey: cursorKey[:], ChallengeZone: opts.Config.Certificates.ChallengeZone, AllowInsecureDevelopment: opts.Config.Environment == config.EnvironmentDevelopment,
		PublicTCPPortMin: int32(opts.Config.Tunnel.PublicTCPPortMin), PublicTCPPortMax: int32(opts.Config.Tunnel.PublicTCPPortMax),
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure tunnel resource service: %w", err)
	}
	domainReconciler, err := tunnelv1.NewDomainReconciler(store, tunnelv1.NetDomainDNSResolver{}, nil)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure tunnel domain reconciler: %w", err)
	}
	previewDomainReconciler, err := previewdomain.NewDNSReconciler(previewDomainRepository, tunnelv1.NetDomainDNSResolver{}, previewdomain.ReconcilerConfig{})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview domain reconciler: %w", err)
	}
	trustedLoginOrigin := opts.Config.HTTP.BrowserLoginOrigin
	if trustedLoginOrigin == "" && len(opts.Config.HTTP.AllowedOrigins) == 1 {
		trustedLoginOrigin = opts.Config.HTTP.AllowedOrigins[0]
	}
	secureAuthCookies := publicURLSecure(opts.Config.HTTP.PublicBaseURL)
	if trustedLoginOrigin != "" {
		secureAuthCookies = publicURLSecure(trustedLoginOrigin)
	}
	if opts.Config.Environment == config.EnvironmentProduction {
		secureAuthCookies = true
	}
	authService := auth.NewService(store, auditWriter, workOSVerifier(opts.Config), opts.Config.Secrets.SessionKeys, secureAuthCookies, trustedLoginOrigin)
	authService.RequireWorkOSSessions(opts.Config.Secrets.EncryptionKey)
	if err := authService.ConfigureSessions(opts.Config.BrowserSessions); err != nil {
		_ = store.Close()
		return nil, err
	}
	// Operator credentials and sessions are independent of customer WorkOS identities.
	deviceAuthService := auth.NewDeviceService(store, auditWriter, opts.Config.CLIAuth, opts.Config.Secrets.SessionKeys)
	deviceAuthService.ConfigureAuthorizationEncryptionKey(opts.Config.Secrets.EncryptionKey)
	var stripeClient billing.StripeClient
	if opts.Config.Secrets.StripeAPIKey != "" {
		stripeClient = billing.HTTPStripeClient{BaseURL: opts.Config.Providers.Stripe.BaseURL, APIKey: opts.Config.Secrets.StripeAPIKey}
	}
	workspaceBilling := billing.NewWorkspaceService(store, stripeClient, opts.Config.Secrets.EncryptionKey)
	policyContext, cancelPolicy := context.WithTimeout(context.Background(), opts.Config.HTTP.RequestTimeout)
	policyErr := workspaceBilling.InitializeAllowancePolicy(policyContext, opts.Config.Billing.PersonalAllowanceBytes, opts.Config.Billing.TeamAllowanceBytes, opts.Config.Billing.PersonalMachineLimit)
	cancelPolicy()
	if policyErr != nil {
		_ = store.Close()
		return nil, policyErr
	}
	githubService := pbgithub.NewService(store, auditWriter, githubClient(opts.Config), opts.Config)
	mintKeys, err := mintKeyProvider(opts.Config)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	previewDispatcher, err := previewdispatch.New(previewdispatch.Config{
		Resolver: previewdispatch.DBMachineRouteResolver{DB: store},
		Signer:   mintKeys,
		Issuer:   normalizeHelperIssuer(opts.Config.HTTP.PublicBaseURL),
		Client:   providerHTTPClient("helper", opts.Config.HTTP.RequestTimeout),
		Timeout:  opts.Config.HTTP.RequestTimeout,
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview dispatcher: %w", err)
	}
	previewLeaseService.ConfigureDispatcher(previewDispatcher)
	checker := readinessChecker{cfg: opts.Config, db: store}
	userMachineService := usermachines.New(store, auditWriter, usermachines.Policy{PairingLifetime: opts.Config.UserMachines.PairingLifetime, OfflineAfter: opts.Config.UserMachines.OfflineAfter, AllowedPlatforms: opts.Config.UserMachines.AllowedPlatforms}, workspaceBilling)
	teamInboxService, err := teaminbox.New(store)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure Team Inbox: %w", err)
	}
	teamInboxService.ConfigureEncryptionKey(opts.Config.Secrets.EncryptionKey)
	userMachineService.ConfigureTeamInbox(teamInboxService)
	teamInboxReceiptWorker, err := teaminbox.NewReceiptWorker(teamInboxService, opts.ReceiptEmailSender, time.Second)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure Team Inbox receipt delivery: %w", err)
	}
	userMachineService.ConfigureInstallationEncryption(opts.Config.Secrets.EncryptionKey)
	if len(opts.Config.Secrets.SessionKeys) > 0 {
		userMachineService.ConfigureCLIAuth(opts.Config.CLIAuth.ClientID, opts.Config.CLIAuth.AllowedScopes, opts.Config.CLIAuth.AccessTokenLifetime, opts.Config.CLIAuth.RefreshTokenLifetime, opts.Config.Secrets.SessionKeys[0])
	}
	userMachineService.ConfigureAccess(nil, normalizeHelperIssuer(opts.Config.HTTP.PublicBaseURL), opts.Config.CLIAuth.AccessTokenLifetime)
	userMachineService.ConfigureFileTransfer(accessdescriptor.FileTransferPolicy{
		Revision: opts.Config.Access.FileTransfer.Revision, MaxFileBytes: opts.Config.Access.FileTransfer.MaxFileBytes,
		MaxBatchFiles: opts.Config.Access.FileTransfer.MaxBatchFiles, MaxBatchBytes: opts.Config.Access.FileTransfer.MaxBatchBytes,
		MaxConcurrentTransfers: opts.Config.Access.FileTransfer.MaxConcurrentTransfers, RetentionSeconds: int64(opts.Config.Access.FileTransfer.Retention / time.Second),
		DeliveryTimeoutSeconds: int64(opts.Config.Access.FileTransfer.DeliveryTimeout / time.Second), MaxPendingSpoolBytes: opts.Config.Access.FileTransfer.MaxPendingSpoolBytes,
	})
	userMachineService.ConfigureTerminalSessions(opts.Config.TerminalSessions.MaxActivePerProject, mintKeys, &http.Client{Timeout: opts.Config.TerminalSessions.OperationTimeout})
	userMachineService.ConfigureMachineControl(mintKeys, normalizeHelperIssuer(opts.Config.HTTP.PublicBaseURL))
	publicBaseURL := strings.TrimRight(config.NormalizeIssuer(opts.Config.HTTP.PublicBaseURL), "/")
	releaseBaseURL := strings.TrimRight(config.NormalizeIssuer(opts.Config.ReleaseBaseURL), "/")
	if releaseBaseURL == "" {
		releaseBaseURL = publicBaseURL
	}
	userMachineService.ConfigureBootstrapCommand("curl -fsSL " + releaseBaseURL + "/install | bash -s -- --pair")
	if err := userMachineService.ConfigureRuntimeRoute(opts.Config.RuntimeBaseDomain, opts.Config.UserMachines.RuntimeListenPort); err != nil {
		return nil, err
	}

	var releaseAuthorityService *releaseauthority.Service
	if len(opts.Config.ReleaseAuthority.PublicKeys) > 0 {
		authorityKeys, authorityErr := releaseauthority.ParseKeys(opts.Config.ReleaseAuthority.PublicKeys)
		if authorityErr != nil {
			_ = store.Close()
			return nil, fmt.Errorf("configure release authority keys: %w", authorityErr)
		}
		releaseAuthorityService, authorityErr = releaseauthority.New(store, auditWriter, releaseauthority.Config{Keys: authorityKeys, Threshold: opts.Config.ReleaseAuthority.Threshold})
		if authorityErr != nil {
			_ = store.Close()
			return nil, fmt.Errorf("configure release authority: %w", authorityErr)
		}
	}
	var releaseFileHandler http.Handler
	if strings.TrimSpace(opts.Config.ReleaseDirectory) != "" {
		releaseFileHandler, err = httpapi.NewReleaseFiles(opts.Config.ReleaseDirectory)
		if err != nil {
			return nil, fmt.Errorf("configure release directory: %w", err)
		}
	}
	enrollmentService := controlplane.NewEnrollmentService(store, mintKeys, auditWriter, config.NormalizeIssuer(opts.Config.HTTP.PublicBaseURL), opts.Config.Secrets.EncryptionKey)
	connectorControlStore, err := connectorprotocol.NewSQLControlStore(store, connectorprotocol.SQLControlStoreConfig{})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure connector control store: %w", err)
	}
	connectorPersistentServer, err := connectorprotocol.NewPersistentServer(connectorControlStore, connectorControlStore, connectorprotocol.ServerConfig{Capabilities: connectorprotocol.ProductionCapabilities()})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure connector control server: %w", err)
	}
	connectorRotationDispatcher, err := connectorprotocol.NewRotationDispatcher(connectorprotocol.RotationDispatcherConfig{
		Store: connectorControlStore, VerifyOldProof: connectorControlStore.VerifyRotationOldProof,
		// observedWorker installs the safe, cause-aware progress observer for
		// each real reconciliation pass. Reporting here would duplicate it.
		ReportError: func(error) {},
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure connector rotation dispatcher: %w", err)
	}
	connectorDrainDispatcher, err := connectorprotocol.NewDrainDispatcher(connectorprotocol.DrainDispatcherConfig{
		Store:       connectorControlStore,
		ReportError: func(error) {},
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure connector drain dispatcher: %w", err)
	}
	connectorControlTransport, err := connectorprotocol.NewControlTransportWithDispatchers(connectorPersistentServer, nil, connectorRotationDispatcher, connectorDrainDispatcher)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure connector control transport: %w", err)
	}
	connectorControlHandler, err := connectorprotocol.NewWebSocketHandler(connectorControlTransport)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure connector control websocket: %w", err)
	}
	connectorCarrierBootstrapHandler, err := httpapi.NewConnectorCarrierBootstrapHandler(
		connectorControlTransport.Sessions(),
		connectorprotocol.SQLCarrierBootstrapSource{DB: store, Quota: workspaceBilling},
		enrollmentService,
	)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure connector carrier bootstrap: %w", err)
	}
	userMachineService.ConfigureHelperEnrollment(func(ctx context.Context, actorID, operationKey, environmentID string, lifetime time.Duration) (usermachines.HelperEnrollmentGrant, error) {
		grant, err := enrollmentService.Issue(ctx, actorID, operationKey, environmentID, lifetime)
		return usermachines.HelperEnrollmentGrant{EnrollmentID: grant.EnrollmentID, HelperID: grant.HelperID, Credential: grant.Credential, ExpiresAt: grant.ExpiresAt}, err
	})
	userMachineService.ConfigureHelperRecovery(func(ctx context.Context, actorID, operationKey, environmentID, helperID string, lifetime time.Duration) (usermachines.HelperEnrollmentGrant, error) {
		grant, err := enrollmentService.RecoverHelper(ctx, actorID, operationKey, environmentID, helperID, lifetime)
		return usermachines.HelperEnrollmentGrant{EnrollmentID: grant.EnrollmentID, HelperID: grant.HelperID, Credential: grant.Credential, ExpiresAt: grant.ExpiresAt}, err
	})
	userMachineService.ConfigureAuthenticatedHelperEnrollment(
		func(ctx context.Context, actorID, operationKey, environmentID string, lifetime time.Duration, guard usermachines.HelperEnrollmentAuthorityGuard) (usermachines.HelperEnrollmentGrant, error) {
			return usermachines.PersistAuthenticatedHelperEnrollment(ctx, store, environmentID, guard, func(ctx context.Context) (usermachines.HelperEnrollmentGrant, error) {
				grant, err := enrollmentService.Issue(ctx, actorID, operationKey, environmentID, lifetime)
				return usermachines.HelperEnrollmentGrant{EnrollmentID: grant.EnrollmentID, HelperID: grant.HelperID, Credential: grant.Credential, ExpiresAt: grant.ExpiresAt}, err
			})
		},
		func(ctx context.Context, actorID, operationKey, environmentID, helperID string, lifetime time.Duration, guard usermachines.HelperEnrollmentAuthorityGuard) (usermachines.HelperEnrollmentGrant, error) {
			return usermachines.PersistAuthenticatedHelperEnrollment(ctx, store, environmentID, guard, func(ctx context.Context) (usermachines.HelperEnrollmentGrant, error) {
				grant, err := enrollmentService.RecoverHelper(ctx, actorID, operationKey, environmentID, helperID, lifetime)
				return usermachines.HelperEnrollmentGrant{EnrollmentID: grant.EnrollmentID, HelperID: grant.HelperID, Credential: grant.Credential, ExpiresAt: grant.ExpiresAt}, err
			})
		},
	)
	configAssignmentService := controlplane.NewConfigAssignmentService(store, auditWriter, opts.Config.ConfigSync.WarningRevision)
	configAssignmentService.SetRepositoryResolver(controlplane.ConfigRepositoryResolverFunc(func(ctx context.Context, userID, provider, externalID string) (controlplane.ConfigRepositoryConnection, error) {
		if provider != "github" || githubService == nil {
			return controlplane.ConfigRepositoryConnection{}, controlplane.ErrAssignmentForbidden
		}
		repository, accountID, resolveErr := githubService.ResolvePrivateRepository(ctx, userID, externalID)
		if resolveErr != nil {
			return controlplane.ConfigRepositoryConnection{}, resolveErr
		}
		return controlplane.ConfigRepositoryConnection{
			ProviderAccountID: accountID, ExternalRepositoryID: repository.ID,
			DisplayName: repository.Owner + "/" + repository.Name, CloneURL: repository.CloneURL, PublishURL: repository.CloneURL,
			DefaultBranch: repository.DefaultBranch, AuthorizationRef: "github-user:" + userID,
			CredentialCapability: "provider_user_repository",
		}, nil
	}))
	configAssignmentService.SetRepositoryCatalog(controlplane.ConfigRepositoryCatalogPager{List: func(ctx context.Context, userID string) ([]controlplane.ConfigRepositoryCandidate, error) {
		if githubService == nil {
			return nil, controlplane.ErrAssignmentForbidden
		}
		repositories, listErr := githubService.ListRepos(ctx, userID)
		if listErr != nil {
			return nil, listErr
		}
		items := make([]controlplane.ConfigRepositoryCandidate, 0, len(repositories))
		for _, repository := range repositories {
			if !repository.Private || repository.ID == "" || repository.Owner == "" || repository.Name == "" {
				continue
			}
			items = append(items, controlplane.ConfigRepositoryCandidate{
				Provider: "github", ExternalID: repository.ID,
				DisplayName: repository.Owner + "/" + repository.Name, DefaultBranch: repository.DefaultBranch,
			})
		}
		return items, nil
	}, Page: func(ctx context.Context, userID, cursor string, limit int) ([]controlplane.ConfigRepositoryCandidate, string, error) {
		if githubService == nil {
			return nil, "", controlplane.ErrAssignmentUnavailable
		}
		repositories, next, err := githubService.RepositoryInventory(ctx, userID, cursor, limit, true)
		if err != nil {
			return nil, "", err
		}
		items := make([]controlplane.ConfigRepositoryCandidate, 0, len(repositories))
		for _, repository := range repositories {
			items = append(items, controlplane.ConfigRepositoryCandidate{Provider: "github", ExternalID: repository.ID, DisplayName: repository.Owner + "/" + repository.Name, DefaultBranch: repository.DefaultBranch})
		}
		return items, next, nil
	}})
	configCredentialService := controlplane.NewConfigCredentialService(store, mintKeys, config.NormalizeIssuer(opts.Config.HTTP.PublicBaseURL), opts.Config.Secrets.EncryptionKey)
	configCredentialService.SetAuditWriter(auditWriter)
	configCredentialService.SetWarningRevision(opts.Config.ConfigSync.WarningRevision)
	configCredentialService.SetMode(opts.Config.ConfigSync.Mode)
	configLeaseService := controlplane.NewConfigLeaseService(store, auditWriter)
	configLeaseService.ConfigureAuthentication(enrollmentService, mintKeys, config.NormalizeIssuer(opts.Config.HTTP.PublicBaseURL), opts.Config.ConfigSync.WarningRevision)
	configLeaseService.ConfigureMode(opts.Config.ConfigSync.Mode)
	configStatusService := controlplane.NewConfigStatusService(store, enrollmentService, auditWriter, opts.Config.ConfigSync.SummaryLimit)
	configStatusService.SetAccountPolicy(opts.Config.ConfigSync)
	configRepositoryAccessService := controlplane.NewConfigRepositoryAccessService(store, configLeaseService,
		controlplane.ConfigRepositoryAccessIssuerFuncs{
			Issue: func(ctx context.Context, userID, repositoryID, contentsPermission string) (controlplane.ScopedRepositoryCredential, error) {
				issued, issueErr := githubService.IssueUserRepositoryAccess(ctx, userID, repositoryID, contentsPermission)
				return controlplane.ScopedRepositoryCredential{Token: issued.Token, ExpiresAt: issued.ExpiresAt}, issueErr
			},
		}, opts.Config.Secrets.EncryptionKey, auditWriter)
	configRuntimeService := controlplane.NewConfigRuntimeService(store, configLeaseService, opts.Config.ConfigSync)
	configConflictService := controlplane.NewConfigConflictService(store, configLeaseService, auditWriter)
	routeService := controlplane.NewRouteService(store, auditWriter)
	controlDiagnostics := controlplane.NewDiagnosticsService(store)
	operationRecovery := controlplane.NewOperationRecoveryService(store, auditWriter)
	workspaceRecovery := billing.NewWorkspaceRecoveryService(store, auditWriter, workspaceBilling)
	peerIdentityRepository, err := peeridentity.NewSQLRepository(store, auditWriter)
	if err != nil {
		return nil, err
	}
	peerIdentityService, err := peeridentity.NewService(peerIdentityRepository)
	if err != nil {
		return nil, err
	}
	// Environment management reuses only the public account-root resolver. The general
	// server encryption key is deliberately never passed to ENV code.
	environmentVariableService := environment.NewService(store, auditWriter, peerIdentityService, config.NormalizeIssuer(opts.Config.HTTP.PublicBaseURL))
	peerNetworkService, err := peersessions.NewNetworkService(store, mintKeys, config.NormalizeIssuer(opts.Config.HTTP.PublicBaseURL), peersessions.NetworkOptions{STUNServers: opts.Config.Network.STUNServers})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure peer network authority: %w", err)
	}
	nativePrivateResolver, err := nativeprivateaccess.NewSQLResolver(store)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure native private target resolver: %w", err)
	}
	nativePrivateService := &nativeprivateaccess.Service{Resolver: nativePrivateResolver, Signer: mintKeys, Issuer: config.NormalizeIssuer(opts.Config.HTTP.PublicBaseURL)}
	managedSSHRepository, err := managedssh.NewSQLRepository(store, auditWriter)
	if err != nil {
		return nil, err
	}
	managedSSHService, err := managedssh.NewService(managedSSHRepository)
	if err != nil {
		return nil, err
	}
	var diagnosticUploadService *diagnosticuploads.Service
	if opts.Config.Diagnostics.ObjectEndpoint != "" {
		diagnosticRepository, repositoryErr := diagnosticuploads.NewSQLRepository(store, auditWriter)
		if repositoryErr != nil {
			_ = store.Close()
			return nil, repositoryErr
		}
		objectContext, cancelObjects := context.WithTimeout(context.Background(), opts.Config.HTTP.RequestTimeout)
		objectStore, objectErr := diagnosticuploads.NewS3ObjectStore(objectContext, opts.Config.Diagnostics, opts.Config.Secrets.DiagnosticsAccessKey, opts.Config.Secrets.DiagnosticsSecretKey, &http.Client{Timeout: opts.Config.HTTP.RequestTimeout})
		cancelObjects()
		if objectErr != nil {
			_ = store.Close()
			return nil, objectErr
		}
		diagnosticUploadService, err = diagnosticuploads.New(diagnosticRepository, objectStore)
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		if err = diagnosticUploadService.SetRetention(opts.Config.Diagnostics.Retention); err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	selfhostService := selfhost.New(store, opts.Config.Secrets.EncryptionKey)
	selfhostService.ControlURL = publicBaseURL
	selfhostService.TrustDocuments = func(ctx context.Context) ([]byte, []byte, error) {
		keys, err := mintKeys.JWKSJSON()
		if err != nil {
			return nil, nil, err
		}
		service := controlplane.NewEdgeService(store, opts.Config.Secrets.EdgeControlCredential)
		revocations, err := service.Revocations(ctx)
		if err != nil {
			return nil, nil, err
		}
		raw, err := json.Marshal(revocations)
		return keys, raw, err
	}
	infrastructureService := operatorinfra.NewService(store, opts.Config.Secrets.EncryptionKey)
	nodeCredentials := nodeauth.Composite{infrastructureService, selfhostService}
	var edgeControlHandler http.Handler
	var edgeControlService *controlplane.EdgeService
	if opts.Config.Secrets.EdgeControlCredential != "" {
		edgeControlService = controlplane.NewEdgeService(store, opts.Config.Secrets.EdgeControlCredential)
		edgeControlService.SetNodeCredentials(nodeCredentials)
		edgeControlService.SetCertificateDistribution(opts.CertificateDistribution)
		edgeControlService.SetScopedBandwidthDebiter(workspaceBilling)
		edgeControlService.SetAuditWriter(auditWriter)
		edgeControlService.SetCredentialIssuer(mintKeys, config.NormalizeIssuer(opts.Config.HTTP.PublicBaseURL), opts.Config.Secrets.EncryptionKey)
		edgeControlService.SetFileTransferPolicy(mint.FileTransferPolicy{Revision: opts.Config.Access.FileTransfer.Revision, MaxFileBytes: opts.Config.Access.FileTransfer.MaxFileBytes, MaxBatchFiles: opts.Config.Access.FileTransfer.MaxBatchFiles, MaxBatchBytes: opts.Config.Access.FileTransfer.MaxBatchBytes, MaxConcurrentTransfers: opts.Config.Access.FileTransfer.MaxConcurrentTransfers, RetentionSeconds: int64(opts.Config.Access.FileTransfer.Retention / time.Second), DeliveryTimeoutSeconds: int64(opts.Config.Access.FileTransfer.DeliveryTimeout / time.Second), MaxPendingSpoolBytes: opts.Config.Access.FileTransfer.MaxPendingSpoolBytes})
		edgeControlHandler = edgeControlService.Handler()
	}
	previewAttachmentMachineVerifier, err := httpapi.NewPreviewAttachmentMachineProofVerifier(enrollmentService)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview carrier machine verifier: %w", err)
	}
	previewAttachmentLeasePrecondition, err := httpapi.NewPreviewAttachmentLeasePreconditionChecker(previewTunnelStore)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview carrier lease precondition: %w", err)
	}
	previewAttachmentHandler, err := previewattachment.NewHTTPHandler(previewAttachmentProduction.Service, previewAttachmentMachineVerifier, previewAttachmentLeasePrecondition)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview carrier host handler: %w", err)
	}
	lazyActivator := &lazyaccess.Activator{DB: store, Leases: previewLeaseService}
	inspectorHandlers := &httpapi.InspectorHandlers{Access: inspectaccess.NewService(store), Machine: previewAttachmentMachineVerifier}
	var browserAccessHandlers *httpapi.BrowserAccessHandlers
	var previewEdgeAttachmentHandler http.Handler
	var runtimeAttachmentHandler http.Handler
	var privateAccessAuthorizeHandler http.Handler
	var privateAccessGrantHandler http.Handler
	var privateAccessRoutesHandler http.Handler
	if opts.Config.Secrets.EdgeControlCredential != "" {
		previewEdgeVerifier, verifierErr := previewattachment.NewDBPreviewEdgeRequestVerifier(store, opts.Config.Secrets.EdgeControlCredential)
		if verifierErr != nil {
			_ = store.Close()
			return nil, fmt.Errorf("configure preview carrier edge verifier: %w", verifierErr)
		}
		previewEdgeVerifier.SetNodeCredentials(nodeCredentials)
		runtimeAttachmentHandler = &runtimeattachment.Handler{Service: &runtimeattachment.Service{DB: store}, Machine: previewAttachmentMachineVerifier, Edge: previewEdgeVerifier}
		if trustedLoginOrigin != "" {
			newBrowserService := browseraccess.NewService
			if opts.Config.Environment != config.EnvironmentProduction {
				newBrowserService = browseraccess.NewDevelopmentService
			}
			browserService, browserErr := newBrowserService(store, trustedLoginOrigin)
			if browserErr != nil {
				_ = store.Close()
				return nil, fmt.Errorf("configure browser authority: %w", browserErr)
			}
			if err := browserService.ConfigureSessions(opts.Config.BrowserSessions); err != nil {
				_ = store.Close()
				return nil, err
			}
			browserService.MachineServiceDomain = opts.Config.Preview.BaseDomain
			browserAccessHandlers = &httpapi.BrowserAccessHandlers{Lazy: lazyActivator, Access: browserService, Ingress: &browseringress.Service{DB: store, Access: browserService, Quota: workspaceBilling}, Edge: previewEdgeVerifier, Machine: previewAttachmentMachineVerifier, ViewerPolicyReconciler: edgeControlService}
		}
		previewEdgeHandler, edgeHandlerErr := previewattachment.NewEdgeHTTPHandler(previewAttachmentProduction.Service, previewAttachmentProduction.Repository, previewEdgeVerifier)
		err = edgeHandlerErr
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("configure preview carrier edge handler: %w", err)
		}
		previewCertificateReadiness, readinessErr := tunnelcert.NewSQLStore(store)
		if readinessErr != nil {
			_ = store.Close()
			return nil, fmt.Errorf("configure preview certificate readiness: %w", readinessErr)
		}
		previewAliasProjector, projectorErr := previewdomain.NewPreviewCarrierAliasProjector(previewDomainRepository, previewCertificateReadiness, nil)
		if projectorErr != nil {
			_ = store.Close()
			return nil, fmt.Errorf("configure preview carrier aliases: %w", projectorErr)
		}
		if err = previewEdgeHandler.SetAliasProjector(previewAliasProjector); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("attach preview carrier aliases: %w", err)
		}
		previewEdgeAttachmentHandler = previewEdgeHandler
		privateAccessProduction, productionErr := privateaccess.NewProduction(
			store, previewAttachmentProduction.Repository, mintKeys,
			config.NormalizeIssuer(opts.Config.HTTP.PublicBaseURL), enrollmentService,
			privateaccess.PreviewEdgeVerifierAdapter{Verifier: previewEdgeVerifier},
			privateaccess.AuditWriterSink{Writer: auditWriter}, workspaceBilling,
		)
		if productionErr != nil {
			_ = store.Close()
			return nil, fmt.Errorf("configure private access: %w", productionErr)
		}
		if browserAccessHandlers != nil {
			browserAccessHandlers.Ingress.Native = privateAccessProduction.Service
		}
		privateAccessAuthorizeHandler = privateAccessProduction.AuthorizeHTTP
		privateAccessGrantHandler = privateAccessProduction.GrantIssueHTTP
		privateAccessRoutesHandler = privateAccessProduction.AccessorRoutes
	}
	telemetryMetrics := telemetry.NewMetrics()
	telemetryHealth, telemetryErr := telemetry.NewHealthTracker(time.Now)
	if telemetryErr != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure telemetry health: %w", telemetryErr)
	}
	telemetryHTTP := &telemetry.HTTPObserver{
		Metrics: telemetryMetrics,
		Identity: func(ctx context.Context) (string, string) {
			return serverdiagnostics.RequestID(ctx), serverdiagnostics.CorrelationID(ctx)
		},
	}
	telemetryProducer := &telemetry.Producer{Metrics: telemetryMetrics, Health: telemetryHealth, Now: time.Now}
	// Connector protocol persistence owns the live session lifecycle. Attach
	// telemetry after the producer is composed; the adapter is best-effort and
	// never changes protocol persistence outcomes.
	connectorPersistentServer.SetTelemetryProducer(telemetryProducer)
	telemetryDiagnostics, telemetryErr := telemetry.NewDiagnostics(telemetry.DiagnosticsConfig{
		Metrics: telemetryMetrics,
		Health:  telemetryHealth,
		Now:     time.Now,
	})
	if telemetryErr != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure telemetry diagnostics: %w", telemetryErr)
	}
	if err = previewDomainReconciler.SetEventSink(previewDNSReconcileTelemetry(telemetryProducer)); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure preview DNS telemetry: %w", err)
	}
	if err = domainReconciler.SetTelemetryObserver(tunnelDNSReconcileTelemetry(telemetryProducer)); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure tunnel DNS telemetry: %w", err)
	}
	if opts.CertificateWorker != nil {
		if err = opts.CertificateWorker.SetTelemetryObserver(certificateTelemetry(telemetryProducer)); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("configure certificate telemetry: %w", err)
		}
	}
	if certificateRuntime != nil && certificateRuntime.PlatformWorker != nil {
		if err = certificateRuntime.PlatformWorker.SetTelemetryObserver(certificateTelemetry(telemetryProducer)); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("configure platform certificate telemetry: %w", err)
		}
	}
	machineServices := machineservices.New(store, opts.Config.LocalAccessBaseDomain)
	supportStore, err := supportreport.NewSQLStore(store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	supportService, err := supportreport.New(supportStore, supportreport.Config{Enabled: opts.Config.Reporting.Enabled, DSN: opts.Config.Secrets.SentryDSN, Release: opts.Config.Reporting.Release, Environment: string(opts.Config.Environment), Region: opts.Config.Reporting.Region, Instance: opts.Config.Reporting.Instance, LogsEnabled: opts.Config.Reporting.LogsEnabled, MetricsEnabled: opts.Config.Reporting.MetricsEnabled, TracesSampleRate: opts.Config.Reporting.TracesSampleRate, PlatformMetricNames: serverdiagnostics.MetricNames()})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	supportService.SetLogger(opts.Logger)
	reporterTransferred := false
	healthMonitor, err := jobmonitor.New(jobmonitor.Config{Enabled: opts.Config.Reporting.Enabled && opts.Config.Reporting.HealthMonitor != "", DSN: opts.Config.Secrets.SentryDSN, Release: opts.Config.Reporting.Release, Environment: string(opts.Config.Environment), Slug: opts.Config.Reporting.HealthMonitor, Logger: opts.Logger})
	if err != nil {
		supportService.Close()
		_ = store.Close()
		return nil, err
	}
	defer func() {
		if !reporterTransferred {
			healthMonitor.Close()
			supportService.Close()
		}
	}()
	store.SetQueryObserver(func(ctx context.Context) (context.Context, func(string)) {
		return supportService.Begin(ctx, "database_operation", "", "")
	})
	supportService.SetSnapshot(func() {
		supportService.RegistrySnapshot(telemetryMetrics.Snapshot())
		supportService.PlatformSnapshot(serverdiagnostics.MetricsSnapshot())
		stats := store.Pool().Stat()
		supportService.Gauge("paperboat.database.connections", float64(stats.TotalConns()))
		supportService.Gauge("paperboat.database.acquired", float64(stats.AcquiredConns()))
		supportService.Gauge("paperboat.database.idle", float64(stats.IdleConns()))
		supportService.Gauge("paperboat.database.acquire_wait_seconds_snapshot", stats.AcquireDuration().Seconds())
		supportService.Gauge("paperboat.database.acquire_canceled_snapshot", float64(stats.CanceledAcquireCount()))
		supportService.Gauge("paperboat.runtime.goroutines", float64(runtime.NumGoroutine()))
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		supportService.Gauge("paperboat.runtime.heap_bytes", float64(memory.HeapAlloc))
	})
	backupService, err := backupreport.NewService(store, backupreport.Config{DatabaseID: opts.Config.Backups.DatabaseID, Token: opts.Config.Secrets.BackupReportToken, MaxAge: time.Duration(opts.Config.Backups.MaxAgeSeconds) * time.Second, RestoreMaxAge: time.Duration(opts.Config.Backups.RestoreMaxAgeSeconds) * time.Second})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	reportingService, err := operatorreporting.New(store, auditWriter, operatorreporting.Config{BaseURL: opts.Config.Reporting.APIBaseURL, Organization: opts.Config.Reporting.Organization, Projects: opts.Config.Reporting.Projects, Token: opts.Config.Secrets.SentryAPIToken})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	operatorService := operator.NewService(store)
	if err := operatorService.SetHostStaleAfter(opts.Config.UserMachines.OfflineAfter); err != nil {
		_ = store.Close()
		return nil, err
	}
	resourceUsageService := resourceusage.New(store)
	browserTerminalGateway := &browserterminal.Gateway{
		RecordUsageAdmission: resourceUsageService.RecordBrowserAdmission,
		Tickets:              browserterminal.TicketStore{DB: store},
		Authorizer:           browserterminal.MachineAuthorizer{Service: userMachineService},
		Signer:               mintKeys,
		Issuer:               normalizeHelperIssuer(opts.Config.HTTP.PublicBaseURL),
		AllowedOrigins:       append([]string(nil), opts.Config.HTTP.AllowedOrigins...),
	}
	if edgeControlService != nil {
		edgeControlService.SetBrowserTerminalGateway(browserTerminalGateway)
	}
	var priceReader billing.PriceReader
	if reader, ok := stripeClient.(billing.PriceReader); ok {
		priceReader = reader
	}
	router := httpapi.NewRouter(httpapi.Options{
		ClientUpdatePolicy:        clientupdate.New(store, auditWriter, opts.Config.ReleaseDirectory),
		SupportReports:            supportService,
		OperatorInfrastructure:    infrastructureService,
		OperatorBackups:           backupService,
		OperatorReporting:         reportingService,
		Operator:                  operatorService,
		OperatorAuth:              &operatorauth.Service{Store: store, BootstrapEmail: opts.Config.OperatorBootstrapEmail},
		OperatorHealth:            telemetryHealth,
		OperatorActions:           operatoractions.NewService(store),
		OperatorBilling:           billing.NewOperatorService(store, priceReader),
		OperatorAudit:             auditWriter,
		Selfhost:                  &httpapi.SelfhostHandlers{Service: selfhostService},
		Config:                    opts.Config,
		Logger:                    opts.Logger,
		ReadinessChecker:          checker,
		Auth:                      authService,
		DeviceAuth:                deviceAuthService,
		WorkspaceBilling:          workspaceBilling,
		WorkspaceBillingRecovery:  workspaceRecovery,
		GitHub:                    githubService,
		EnvironmentVariables:      environmentVariableService,
		Teams:                     teams.NewService(store),
		TeamInbox:                 teamInboxService,
		MeteringRepo:              metering.NewRuntimeRepository(store, opts.Config.Secrets.EncryptionKey, opts.Config.ConfigSync.StaleHeartbeatAfter),
		RuntimeIdentity:           enrollmentService,
		Machines:                  userMachineService,
		Workspaces:                workspace.New(store),
		BrowserTerminal:           browserTerminalGateway,
		MintKeys:                  mintKeys,
		EdgeControl:               edgeControlHandler,
		EdgeControlAdmin:          edgeControlService,
		Enrollment:                enrollmentService,
		ConfigAssignments:         configAssignmentService,
		ConfigCredentials:         configCredentialService,
		ConfigLeases:              configLeaseService,
		ConfigStatuses:            configStatusService,
		ConfigRepositoryAccess:    configRepositoryAccessService,
		ConfigRuntime:             configRuntimeService,
		ConfigConflicts:           configConflictService,
		Routes:                    routeService,
		Favorites:                 favorites.NewService(store),
		ControlDiagnostics:        controlDiagnostics,
		OperationRecovery:         operationRecovery,
		PeerIdentity:              peerIdentityService,
		PeerNetwork:               peerNetworkService,
		MachineServices:           machineServices,
		ResourceUsage:             resourceUsageService,
		NativePrivateAccess:       nativePrivateService,
		ManagedSSH:                managedSSHService,
		DiagnosticUploads:         diagnosticUploadService,
		ReleaseFiles:              releaseFileHandler,
		ReleaseAuthority:          releaseAuthorityService,
		PreviewTunnelAPI:          previewTunnelAPI,
		LazyPolicies:              lazyPolicyService,
		LazyActivation:            lazyActivator,
		PreviewLeases:             previewLeaseService,
		PreviewDomains:            previewDomainService,
		Tunnels:                   tunnelService,
		TunnelResources:           tunnelResourceService,
		PreviewLogs:               tunnelResourceService,
		PreviewCarrierAttachment:  previewAttachmentHandler,
		RuntimeCarrierAttachment:  runtimeAttachmentHandler,
		PreviewCarrierEdge:        previewEdgeAttachmentHandler,
		BrowserAccess:             browserAccessHandlers,
		Inspector:                 inspectorHandlers,
		PrivateAccessAuthorize:    privateAccessAuthorizeHandler,
		PrivateAccessGrant:        privateAccessGrantHandler,
		PrivateAccessRoutes:       privateAccessRoutesHandler,
		ConnectorControl:          connectorControlHandler,
		ConnectorCarrierBootstrap: connectorCarrierBootstrapHandler,
		TelemetryHTTP:             telemetryHTTP,
		TelemetryMetrics:          telemetryMetrics,
		TelemetryDiagnostics:      telemetryDiagnostics,
	})
	// Membership is authoritative; provider retries run outside its transaction.
	progress := workers.NewProgress(opts.Config.Reporting.WorkerStallTimeout)
	serverWorkers := []workers.Worker{
		operationalHealthWorker(backupService, supportService, healthMonitor, progress),
		observedWorker(supportService, "worker_machines", userMachineService.Worker(opts.Config.TerminalSessions.WorkerInterval), progress),
		observedWorker(supportService, "worker_config_warnings", configAssignmentService.WarningReconciliationWorker(opts.Config.TerminalSessions.WorkerInterval), progress),
		observedWorker(supportService, "worker_config_revocation", configRepositoryAccessService.RevocationWorker(opts.Config.TerminalSessions.WorkerInterval, 25), progress),
		observedWorker(supportService, "worker_preview_leases", previewLeaseReconciliationWorker(previewLeaseService, opts.Config.TerminalSessions.WorkerInterval, telemetryProducer), progress),
		observedWorker(supportService, "worker_tunnel_expiry", tunnelExpiryReconciliationWorker(tunnelService, opts.Config.TerminalSessions.WorkerInterval), progress),
		observedWorker(supportService, "worker_connector_rotation", connectorRotationDispatcher.Run, progress),
		observedWorker(supportService, "worker_connector_drain", connectorDrainDispatcher.Run, progress),
		observedWorker(supportService, "worker_domain_dns", domainReconciler.Worker(opts.Config.TerminalSessions.WorkerInterval, 100), progress),
		observedWorker(supportService, "worker_preview_dns", previewDomainReconciler.Worker(opts.Config.TerminalSessions.WorkerInterval, 100), progress),
		observedWorker(supportService, "worker_preview_outbox", previewAttachmentProduction.Repository.OutboxWorker(opts.Config.TerminalSessions.WorkerInterval), progress),
	}
	serverWorkers = append(serverWorkers, observedWorker(supportService, "worker_team_retention", periodicPreviewTunnelWorker(time.Minute, func(ctx context.Context) error {
		pruneContext, cancel := context.WithTimeout(ctx, opts.Config.HTTP.RequestTimeout)
		defer cancel()
		_, err := teams.NewService(store).PruneActivity(pruneContext)
		workers.IterationFailure(ctx, "team_retention_cleanup", "team_retention_failed", err)
		outcome := "success"
		if err != nil {
			outcome = "failed"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			outcome = "canceled"
		}
		supportService.Operation(ctx, "worker_team_retention", outcome, "", -1)
		return ctx.Err()
	}), progress))
	serverWorkers = append(serverWorkers, observedWorker(supportService, "worker_team_entitlements", periodicPreviewTunnelWorker(opts.Config.TerminalSessions.WorkerInterval, func(ctx context.Context) error {
		expiryContext, cancel := context.WithTimeout(ctx, opts.Config.HTTP.RequestTimeout)
		defer cancel()
		_, err := store.SQL().ExecContext(expiryContext, `SELECT paperboat.expire_team_paid_entitlements()`)
		workers.IterationFailure(ctx, "team_entitlement_expiry", "team_entitlements_failed", err)
		outcome := "success"
		if err != nil {
			outcome = "failed"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			outcome = "canceled"
		}
		supportService.Operation(ctx, "worker_team_entitlements", outcome, "", -1)
		return ctx.Err()
	}), progress))
	if stripeClient != nil {
		serverWorkers = append(serverWorkers, observedWorker(supportService, "worker_team_billing", periodicPreviewTunnelWorker(time.Minute, func(ctx context.Context) error {
			billingContext, cancel := context.WithTimeout(ctx, opts.Config.HTTP.RequestTimeout)
			defer cancel()
			err := workspaceBilling.ReconcileMemberships(billingContext, teams.MaximumMembers)
			workers.IterationFailure(ctx, "team_billing_reconcile", "team_billing_failed", err)
			outcome := "success"
			if err != nil {
				outcome = "failed"
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				outcome = "canceled"
			}
			supportService.Operation(ctx, "worker_team_billing", outcome, "", -1)
			return ctx.Err()
		}), progress))
	}
	if teamInboxReceiptWorker != nil {
		serverWorkers = append(serverWorkers, observedWorker(supportService, "worker_team_inbox", teamInboxReceiptWorker.Run, progress))
	}
	if edgeControlService != nil {
		serverWorkers = append(serverWorkers, observedWorker(supportService, "worker_edge_assignments", edgeControlService.TunnelEdgeAssignmentWorker(opts.Config.TerminalSessions.WorkerInterval, 100), progress))
		serverWorkers = append(serverWorkers, observedWorker(supportService, "worker_stale_nodes", edgeControlService.StaleNodeWorker(opts.Config.TerminalSessions.WorkerInterval, controlplane.ControlTunnelNodeStaleAfter()), progress))
	}
	if opts.Config.Certificates.Enabled && opts.Config.Certificates.DNSZoneID != "" {
		dnsWorker, dnsErr := newEdgeDNSWorker(store, opts.Config)
		if dnsErr != nil {
			_ = store.Close()
			return nil, dnsErr
		}
		serverWorkers = append(serverWorkers, observedWorker(supportService, "worker_edge_dns", dnsWorker, progress))
	}
	if diagnosticUploadService != nil {
		serverWorkers = append(serverWorkers, observedWorker(supportService, "worker_diagnostic_uploads", diagnosticUploadService.Worker(time.Minute), progress))
	}
	if opts.CertificateWorker != nil {
		serverWorkers = append(serverWorkers, observedWorker(supportService, "worker_certificates", opts.CertificateWorker.Worker(opts.Config.TerminalSessions.WorkerInterval, 100), progress))
	}
	if certificateRuntime != nil && certificateRuntime.PlatformWorker != nil {
		serverWorkers = append(serverWorkers, observedWorker(supportService, "worker_platform_certificates", certificateRuntime.PlatformWorker.Worker(opts.Config.TerminalSessions.WorkerInterval, 3), progress))
	}
	var syncServer *syncpkg.Server
	if opts.Config.Sync.Address != "" {
		var syncErr error
		certificate, certErr := tls.LoadX509KeyPair(opts.Config.Sync.TLSCertFile, opts.Config.Sync.TLSKeyFile)
		if certErr != nil {
			_ = store.Close()
			return nil, fmt.Errorf("load sync TLS certificate: %w", certErr)
		}
		syncService, serviceErr := syncpkg.NewService(deviceAuthService, &syncpkg.SQLAuthority{DB: store, Services: machineServices})
		if serviceErr != nil {
			_ = store.Close()
			return nil, serviceErr
		}
		syncService.SetSupportReporter(supportService)
		syncServer, syncErr = syncpkg.StartServer(opts.Config.Sync.Address, syncService, certificate)
		if syncErr != nil {
			_ = store.Close()
			return nil, fmt.Errorf("start control sync server on %s: %w", opts.Config.Sync.Address, syncErr)
		}
	}

	application := &App{
		supportReports: supportService,
		selfhost:       selfhostService,
		healthMonitor:  healthMonitor,
		cfg:            opts.Config,
		logger:         opts.Logger,
		db:             store,
		syncServer:     syncServer,
		server: &http.Server{
			Addr:              opts.Config.HTTP.Address,
			Handler:           router,
			ReadHeaderTimeout: opts.Config.HTTP.ReadHeaderTimeout,
		},
		worker:               workers.NewSupervisor(serverWorkers...),
		certificateRuntime:   certificateRuntime,
		telemetryMetrics:     telemetryMetrics,
		telemetryHealth:      telemetryHealth,
		telemetryHTTP:        telemetryHTTP,
		telemetryDiagnostics: telemetryDiagnostics,
		telemetryProducer:    telemetryProducer,
	}
	reporterTransferred = true
	runtimeTransferred = true
	return application, nil
}
