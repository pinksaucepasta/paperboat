package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/activity"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/backupreport"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/browserterminal"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/diagnosticuploads"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
	"github.com/pinksaucepasta/paperboat-server/internal/favorites"
	pbgithub "github.com/pinksaucepasta/paperboat-server/internal/github"
	"github.com/pinksaucepasta/paperboat-server/internal/lazyaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat-server/internal/managedssh"
	"github.com/pinksaucepasta/paperboat-server/internal/metering"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/nativeprivateaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/operator"
	"github.com/pinksaucepasta/paperboat-server/internal/operatoractions"
	"github.com/pinksaucepasta/paperboat-server/internal/operatorauth"
	"github.com/pinksaucepasta/paperboat-server/internal/operatorinfra"
	"github.com/pinksaucepasta/paperboat-server/internal/operatorreporting"
	"github.com/pinksaucepasta/paperboat-server/internal/peeridentity"
	"github.com/pinksaucepasta/paperboat-server/internal/peersessions"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat-server/internal/previewdomain"
	"github.com/pinksaucepasta/paperboat-server/internal/privateaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/releaseauthority"
	"github.com/pinksaucepasta/paperboat-server/internal/resourceusage"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat-server/internal/teaminbox"
	"github.com/pinksaucepasta/paperboat-server/internal/telemetry"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

type ReadinessChecker interface {
	Ready(context.Context) error
}

type Options struct {
	Activity                 *activity.Service
	ClientUpdatePolicy       ClientUpdatePolicyAPI
	OperatorInfrastructure   *operatorinfra.Service
	OperatorBackups          *backupreport.Service
	OperatorReporting        *operatorreporting.Service
	OperatorHealth           *telemetry.HealthTracker
	OperatorActions          *operatoractions.Service
	OperatorBilling          *billing.OperatorService
	Operator                 *operator.Service
	OperatorAuth             *operatorauth.Service
	OperatorAudit            operatorAudit
	SupportReports           *supportreport.Service
	Selfhost                 *SelfhostHandlers
	BrowserAccess            *BrowserAccessHandlers
	Inspector                *InspectorHandlers
	Teams                    teamAPI
	TeamInbox                *teaminbox.Service
	Config                   config.Config
	Logger                   *slog.Logger
	ReadinessChecker         ReadinessChecker
	Auth                     *auth.Service
	DeviceAuth               *auth.DeviceService
	WorkspaceBilling         *billing.WorkspaceService
	WorkspaceBillingRecovery *billing.WorkspaceRecoveryService
	GitHub                   *pbgithub.Service
	EnvironmentVariables     *environment.Service
	MeteringRepo             *metering.RuntimeRepository
	RuntimeIdentity          *controlplane.EnrollmentService
	Machines                 *usermachines.Service
	Workspaces               *workspace.Service
	BrowserTerminal          *browserterminal.Gateway
	MintKeys                 *mint.Provider
	EdgeControl              http.Handler
	EdgeControlAdmin         *controlplane.EdgeService
	Enrollment               *controlplane.EnrollmentService
	ConfigAssignments        *controlplane.ConfigAssignmentService
	ConfigCredentials        *controlplane.ConfigCredentialService
	ConfigLeases             *controlplane.ConfigLeaseService
	ConfigStatuses           *controlplane.ConfigStatusService
	ConfigRepositoryAccess   *controlplane.ConfigRepositoryAccessService
	ConfigRuntime            *controlplane.ConfigRuntimeService
	ConfigConflicts          *controlplane.ConfigConflictService
	Routes                   *controlplane.RouteService
	Favorites                *favorites.Service
	ControlDiagnostics       *controlplane.DiagnosticsService
	OperationRecovery        *controlplane.OperationRecoveryService
	PeerIdentity             *peeridentity.Service
	PeerNetwork              *peersessions.NetworkService
	ResourceUsage            *resourceusage.Service
	NativePrivateAccess      *nativeprivateaccess.Service
	MachineServices          *machineservices.Service
	ManagedSSH               *managedssh.Service
	DiagnosticUploads        *diagnosticuploads.Service
	OverrideHandler          http.Handler
	ReleaseFiles             http.Handler
	ReleaseAuthority         *releaseauthority.Service
	PreviewTunnelAPI         PreviewTunnelAPI
	PreviewLeases            PreviewLeaseAPI
	LazyPolicies             LazyPolicyAPI
	LazyActivation           *lazyaccess.Activator
	PreviewDomains           previewdomain.API
	Tunnels                  tunnelv1.API
	TunnelResources          tunnelv1.ResourceAPI
	PreviewLogs              tunnelv1.PreviewLogAPI
	// PreviewCarrierAttachment is the machine-proof host attachment surface;
	// PreviewCarrierEdge is the edge-control pull/ACK/observation surface.
	RuntimeCarrierAttachment http.Handler
	PreviewCarrierAttachment http.Handler
	PreviewCarrierEdge       http.Handler
	// PrivateAccessAuthorize and PrivateAccessGrant are edge-only handlers.
	// Both first authenticate the edge node/process epoch, then verify the
	// renewable user/machine identity against the current private route.
	PrivateAccessAuthorize    http.Handler
	PrivateAccessGrant        http.Handler
	PrivateAccessRoutes       http.Handler
	ConnectorControl          http.Handler
	ConnectorCarrierBootstrap http.Handler
	TelemetryHTTP             *telemetry.HTTPObserver
	TelemetryMetrics          *telemetry.Metrics
	TelemetryDiagnostics      *telemetry.Diagnostics
}

// tunnelCreateAuth mirrors the preview machine-auth boundary: machine
// requests reach the handler for exact proof verification, while ordinary
// browser and client-session requests retain bearer scope and CSRF checks.
func tunnelCreateAuth(user *auth.Service, machines *auth.DeviceService, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tunnelMachineHeadersPresent(r) {
			next.ServeHTTP(w, r)
			return
		}
		requireAnyAuth(user, machines, requireScope("tunnels:write", requireCSRF(user, next))).ServeHTTP(w, r)
	})
}

// tunnelConnectorEnrollmentAuth is deliberately machine-only. Issuing an
// enrollment creates a one-time connector credential for a host, and
// exchanging it installs that credential on the same host. A browser or CLI
// client-session bearer must never be able to select or impersonate host_id;
// the handler verifies the renewable machine proof over the exact body.
func tunnelConnectorEnrollmentAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tunnelMachineHeadersPresent(r) {
			writePreviewTunnelError(w, r, http.StatusUnauthorized, "machine_identity_required", "A signed host identity is required for connector enrollment.", "unchanged", false, "run_on_authorized_host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func NewRouter(opts Options) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	var handler http.Handler
	if opts.OverrideHandler != nil {
		handler = opts.OverrideHandler
	} else {
		mux := http.NewServeMux()
		if opts.Workspaces != nil && opts.Auth != nil && opts.DeviceAuth != nil {
			mux.Handle("GET /v1/workspaces", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("account:read", requireWorkspaceSelection(opts.Workspaces, workspacesList(opts.Workspaces)))))
		}
		if opts.ClientUpdatePolicy != nil {
			mux.HandleFunc("GET /v1/client/update-policy", clientUpdatePolicyGet(opts.ClientUpdatePolicy))
			if opts.OperatorAuth != nil {
				mux.Handle("GET /v1/admin/client-update-policy", operatorRoute(opts, clientUpdatePolicyGet(opts.ClientUpdatePolicy)))
				mux.Handle("PUT /v1/admin/client-update-policy", operatorRoute(opts, clientUpdatePolicyPut(opts.ClientUpdatePolicy)))
			}
		}
		registerOperatorRoutes(mux, opts)
		registerOperatorAuthentication(mux, opts)
		mux.HandleFunc("GET /healthz", health)
		if opts.ReleaseFiles != nil {
			mux.Handle("GET /install", installScript(opts.ReleaseFiles))
			mux.Handle("GET /selfhost/install", selfhostInstaller(opts.ReleaseFiles))
			mux.Handle("GET /selfhost/versions/{path...}", selfhostPackage(opts.ReleaseFiles))
			mux.HandleFunc("GET /selfhost/address", selfhostAddress)
			mux.Handle("GET /tuf/{path...}", tufRepository("/tuf", opts.ReleaseFiles))
			mux.Handle("GET /helper-releases/tuf/{path...}", tufRepository("/helper-releases/tuf", opts.ReleaseFiles))
		}
		mux.HandleFunc("GET /readyz", ready(opts.ReadinessChecker))
		mux.HandleFunc("GET /v1/client-configuration", clientConfiguration(opts.Config))
		mux.Handle("GET /metrics", metrics(opts.ControlDiagnostics, opts.TelemetryMetrics))
		mux.Handle(telemetryDiagnosticsPath, telemetryDiagnostics(opts.TelemetryDiagnostics))
		if opts.MintKeys != nil {
			mux.Handle("GET /.well-known/jwks.json", opts.MintKeys)
		}
		if opts.PreviewTunnelAPI != nil && opts.Auth != nil && opts.DeviceAuth != nil {
			mux.Handle("GET /v1/operations/{operation_id}", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("operations:read", previewTunnelOperationGet(opts.PreviewTunnelAPI))))
			mux.Handle("DELETE /v1/operations/{operation_id}", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("operations:write", requireCSRF(opts.Auth, previewTunnelOperationCancel(opts.PreviewTunnelAPI)))))
		}
		if opts.PreviewLeases != nil && opts.PreviewTunnelAPI != nil && opts.Auth != nil && opts.DeviceAuth != nil {
			previewRead := func(next http.Handler) http.Handler {
				return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("previews:read", next))
			}
			previewWrite := func(next http.Handler) http.Handler {
				return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("previews:write", requireCSRF(opts.Auth, next)))
			}
			if opts.LazyPolicies != nil {
				registerLazyPolicyRoutes(mux, opts.LazyPolicies, previewRead, previewWrite)
			}
			previewMutation := func(next http.Handler) http.Handler {
				return previewLeaseMutationAuth(opts.Auth, opts.DeviceAuth, opts.RuntimeIdentity, next)
			}
			mux.Handle("POST /v1/previews", previewLeaseCreateAuth(opts.Auth, opts.DeviceAuth, opts.RuntimeIdentity, previewLeaseCreate(opts.PreviewLeases, opts.RuntimeIdentity)))
			mux.Handle("GET /v1/previews", previewRead(requireWorkspaceSelection(opts.Workspaces, previewLeaseList(opts.PreviewLeases))))
			mux.Handle("GET /v1/previews/{preview_id}", previewRead(previewLeaseGet(opts.PreviewLeases)))
			mux.Handle("DELETE /v1/previews/{preview_id}", previewMutation(previewLeaseStop(opts.PreviewLeases, opts.RuntimeIdentity)))
			mux.Handle("POST /v1/previews/{preview_id}/lease/renew", previewMutation(previewLeaseRenew(opts.PreviewLeases, opts.RuntimeIdentity)))
			mux.Handle("GET /v1/previews/{preview_id}/events", previewRead(previewTunnelEvents(opts.PreviewTunnelAPI, "preview_lease", "preview_id")))
			if opts.PreviewLogs != nil {
				mux.Handle("GET /v1/previews/{preview_id}/logs", previewRead(tunnelResourcePreviewLogs(opts.PreviewLogs)))
			}
			if opts.PreviewDomains != nil {
				mux.Handle("POST /v1/previews/{preview_id}/domains", previewWrite(PreviewDomainCreate(opts.PreviewDomains)))
				mux.Handle("GET /v1/previews/{preview_id}/domains", previewRead(PreviewDomainList(opts.PreviewDomains)))
				mux.Handle("GET /v1/previews/{preview_id}/domains/{domain_id}", previewRead(PreviewDomainGet(opts.PreviewDomains)))
				mux.Handle("DELETE /v1/previews/{preview_id}/domains/{domain_id}", previewWrite(PreviewDomainDelete(opts.PreviewDomains)))
				mux.Handle("POST /v1/previews/{preview_id}/domains/{domain_id}/verify", previewWrite(PreviewDomainVerify(opts.PreviewDomains)))
				mux.Handle("GET /v1/previews/{preview_id}/domains/{domain_id}/instructions", previewRead(PreviewDomainInstructions(opts.PreviewDomains)))
			}
		}
		if opts.PreviewLeases != nil && opts.RuntimeIdentity != nil {
			if readiness, ok := opts.PreviewLeases.(PreviewReadinessReadAPI); ok {
				mux.Handle("GET /v1/previews/{preview_id}/readiness", previewLeaseReadinessGet(readiness, opts.RuntimeIdentity))
			}
			if readiness, ok := opts.PreviewLeases.(PreviewReadinessAPI); ok {
				// Readiness is deliberately machine-auth-only. Browser sessions and
				// CLI client-session bearers may observe a preview, but only a
				// renewable signed machine identity can complete the host-owned CAS.
				mux.Handle("POST /v1/previews/{preview_id}/readiness", previewLeaseReadiness(readiness, opts.RuntimeIdentity))
			}
		}
		if opts.EdgeControlAdmin != nil && opts.Auth != nil && opts.DeviceAuth != nil {
			mux.Handle("GET /v1/edges", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("tunnels:read", hostedEdgeList(opts.EdgeControlAdmin))))
		}
		if opts.Tunnels != nil && opts.PreviewTunnelAPI != nil && opts.Auth != nil && opts.DeviceAuth != nil {
			tunnelRead := func(next http.Handler) http.Handler {
				return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("tunnels:read", next))
			}
			tunnelWrite := func(next http.Handler) http.Handler {
				return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("tunnels:write", requireCSRF(opts.Auth, next)))
			}
			// Tunnel creation is the one host-only tunnel mutation. A production
			// host has a renewable machine-control credential, not a DeviceService
			// client session, so let the handler verify its exact machine proof
			// before constructing the host actor.
			mux.Handle("POST /v1/tunnels", tunnelCreateAuth(opts.Auth, opts.DeviceAuth, tunnelCreate(opts.Tunnels, opts.Enrollment)))
			mux.Handle("GET /v1/tunnels", tunnelRead(requireWorkspaceSelection(opts.Workspaces, tunnelList(opts.Tunnels))))
			mux.Handle("GET /v1/tunnels/{tunnel_id}", tunnelRead(tunnelGet(opts.Tunnels)))
			mux.Handle("PATCH /v1/tunnels/{tunnel_id}", tunnelWrite(tunnelPatch(opts.Tunnels)))
			mux.Handle("DELETE /v1/tunnels/{tunnel_id}", tunnelWrite(tunnelDelete(opts.Tunnels)))
			mux.Handle("POST /v1/tunnels/{tunnel_id}/pause", tunnelWrite(tunnelPause(opts.Tunnels)))
			mux.Handle("POST /v1/tunnels/{tunnel_id}/resume", tunnelWrite(tunnelResume(opts.Tunnels)))
			mux.Handle("GET /v1/tunnels/{tunnel_id}/status", tunnelRead(tunnelStatus(opts.Tunnels)))
			mux.Handle("GET /v1/tunnels/{tunnel_id}/events", tunnelRead(previewTunnelEvents(opts.PreviewTunnelAPI, "tunnel", "tunnel_id")))
			if opts.TunnelResources != nil {
				var connectorIdentity machineRequestVerifier
				if opts.Enrollment != nil {
					connectorIdentity = opts.Enrollment
				} else if opts.RuntimeIdentity != nil {
					connectorIdentity = opts.RuntimeIdentity
				}
				mux.Handle("POST /v1/tunnels/{tunnel_id}/routes", tunnelWrite(tunnelResourceRouteCreate(opts.TunnelResources)))
				mux.Handle("GET /v1/tunnels/{tunnel_id}/routes", tunnelRead(tunnelResourceRouteList(opts.TunnelResources)))
				mux.Handle("GET /v1/tunnels/{tunnel_id}/routes/{route_id}", tunnelRead(tunnelResourceRouteGet(opts.TunnelResources)))
				mux.Handle("PATCH /v1/tunnels/{tunnel_id}/routes/{route_id}", tunnelWrite(tunnelResourceRoutePatch(opts.TunnelResources)))
				mux.Handle("DELETE /v1/tunnels/{tunnel_id}/routes/{route_id}", tunnelWrite(tunnelResourceRouteDelete(opts.TunnelResources)))
				mux.Handle("POST /v1/tunnels/{tunnel_id}/domains", tunnelWrite(tunnelResourceDomainCreate(opts.TunnelResources)))
				mux.Handle("GET /v1/tunnels/{tunnel_id}/domains", tunnelRead(tunnelResourceDomainList(opts.TunnelResources)))
				mux.Handle("GET /v1/tunnels/{tunnel_id}/domains/{domain_id}", tunnelRead(tunnelResourceDomainGet(opts.TunnelResources)))
				mux.Handle("DELETE /v1/tunnels/{tunnel_id}/domains/{domain_id}", tunnelWrite(tunnelResourceDomainDelete(opts.TunnelResources)))
				mux.Handle("POST /v1/tunnels/{tunnel_id}/domains/{domain_id}/verify", tunnelWrite(tunnelResourceDomainVerify(opts.TunnelResources)))
				mux.Handle("GET /v1/tunnels/{tunnel_id}/domains/{domain_id}/instructions", tunnelRead(tunnelResourceDomainInstructions(opts.TunnelResources)))
				mux.Handle("GET /v1/tunnels/{tunnel_id}/connectors", tunnelRead(tunnelResourceConnectorList(opts.TunnelResources)))
				mux.Handle("GET /v1/tunnels/{tunnel_id}/connectors/{connector_id}", tunnelRead(tunnelResourceConnectorGet(opts.TunnelResources)))
				mux.Handle("POST /v1/tunnels/{tunnel_id}/connectors/enrollments", tunnelConnectorEnrollmentAuth(tunnelResourceIssueEnrollment(opts.TunnelResources, connectorIdentity)))
				mux.Handle("POST /v1/tunnels/{tunnel_id}/connectors/enrollments/exchange", tunnelConnectorEnrollmentAuth(tunnelResourceExchangeEnrollment(opts.TunnelResources, connectorIdentity)))
				mux.Handle("POST /v1/tunnels/{tunnel_id}/connectors/{connector_id}/drain", tunnelWrite(tunnelResourceConnectorDrain(opts.TunnelResources)))
				mux.Handle("DELETE /v1/tunnels/{tunnel_id}/connectors/{connector_id}", tunnelWrite(tunnelResourceConnectorRevoke(opts.TunnelResources)))
				mux.Handle("POST /v1/tunnels/{tunnel_id}/credentials/rotate", tunnelWrite(tunnelResourceRotateCredentials(opts.TunnelResources)))
				mux.Handle("GET /v1/tunnels/{tunnel_id}/logs", tunnelRead(tunnelResourceTunnelLogs(opts.TunnelResources)))
			}
		}
		if opts.ConnectorControl != nil {
			mux.Handle("GET /v1/tunnels/{tunnel_id}/connectors/{connector_id}/control", opts.ConnectorControl)
		}
		if opts.ConnectorCarrierBootstrap != nil {
			mux.Handle("POST /v1/tunnels/{tunnel_id}/connectors/{connector_id}/carrier-bootstrap", opts.ConnectorCarrierBootstrap)
		}
		if opts.DiagnosticUploads == nil || opts.DeviceAuth == nil {
			unavailable := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeError(w, r, http.StatusServiceUnavailable, "diagnostic_upload_unavailable", "Diagnostic upload service is temporarily unavailable.")
			})
			mux.Handle("POST /v1/diagnostic-upload-intents", unavailable)
			mux.Handle("POST /v1/diagnostic-upload-intents/{intent_id}/complete", unavailable)
		}
		if opts.RuntimeCarrierAttachment != nil {
			mux.Handle("POST /v1/runtime/carrier-attachment", opts.RuntimeCarrierAttachment)
			mux.Handle("POST /v1/edge/runtime/carrier-admissions", opts.RuntimeCarrierAttachment)
			mux.Handle("POST /v1/edge/runtime/carrier-observations", opts.RuntimeCarrierAttachment)
		}
		if opts.PreviewCarrierAttachment != nil {
			mux.Handle("POST /v1/previews/{preview_id}/carrier-attachment", opts.PreviewCarrierAttachment)
			mux.Handle("POST /v1/previews/{preview_id}/carrier-attachment/renew", opts.PreviewCarrierAttachment)
			mux.Handle("POST /v1/previews/{preview_id}/carrier-attachment/readiness", opts.PreviewCarrierAttachment)
			mux.Handle("POST /v1/previews/{preview_id}/carrier-attachment/release", opts.PreviewCarrierAttachment)
		}
		if opts.PreviewCarrierEdge != nil {
			mux.Handle("POST "+previewattachment.EdgeAdmissionPullPath, opts.PreviewCarrierEdge)
			mux.Handle("POST "+previewattachment.EdgeAdmissionAckPath, opts.PreviewCarrierEdge)
			mux.Handle("POST "+previewattachment.EdgeObservationPath, opts.PreviewCarrierEdge)
			mux.Handle("POST "+previewattachment.EdgeDetachmentPath, opts.PreviewCarrierEdge)
		}
		if opts.PrivateAccessGrant != nil {
			mux.Handle("POST "+privateaccess.GrantPath, opts.PrivateAccessGrant)
		}
		if opts.PrivateAccessAuthorize != nil {
			mux.Handle("POST "+privateaccess.AuthorizePath, opts.PrivateAccessAuthorize)
		}
		if opts.PrivateAccessRoutes != nil {
			mux.Handle("POST "+privateaccess.MachineRoutesPath, opts.PrivateAccessRoutes)
			mux.Handle("POST "+privateaccess.EdgeAdmissionsPath, opts.PrivateAccessRoutes)
		}
		if opts.Activity != nil {
			mux.Handle("GET /v1/activity", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("account:read", activityGet(opts.Activity))))
		}

		if opts.ResourceUsage != nil {
			mux.HandleFunc("POST /v1/usage/native", resourceUsageNative(opts.RuntimeIdentity, opts.ResourceUsage))
			mux.Handle("GET /v1/usage", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("account:read", resourceUsageGet(opts.ResourceUsage))))
		}
		if opts.Selfhost != nil {
			mux.HandleFunc("GET /v1/selfhost/runtime-readiness", opts.Selfhost.RuntimeReadiness)
			account := http.HandlerFunc(opts.Selfhost.Account)
			read := requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:read", account))
			write := requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:connect", requireCSRF(opts.Auth, account)))
			mux.Handle("POST /v1/selfhost/enrollments", write)
			mux.Handle("GET /v1/selfhost/installations", read)
			mux.Handle("GET /v1/selfhost/installations/{installationID}", read)
			mux.Handle("DELETE /v1/selfhost/installations/{installationID}", write)
			mux.Handle("POST /v1/selfhost/installations/{installationID}/verify-ingress", write)
			mux.Handle("GET /v1/selfhost/pools/{capability}", read)
			mux.Handle("PUT /v1/selfhost/pools/{capability}", write)
			mux.Handle("GET /v1/selfhost/placements/{resourceKind}/{resourceID}", read)
			mux.Handle("PUT /v1/selfhost/placements/{resourceKind}/{resourceID}", write)
			global := http.HandlerFunc(opts.Selfhost.Global)
			mux.Handle("POST /v1/admin/selfhost/enrollments", operatorRoute(opts, global))
			mux.Handle("GET /v1/admin/selfhost/installations", operatorRoute(opts, global))
			mux.Handle("GET /v1/admin/selfhost/installations/{installationID}", operatorRoute(opts, global))
			mux.Handle("DELETE /v1/admin/selfhost/installations/{installationID}", operatorRoute(opts, global))
		}
		if opts.EdgeControl != nil {
			mux.Handle("/v1/", opts.EdgeControl)
		}
		if opts.Enrollment != nil {
			mux.HandleFunc("POST /v1/helper-enrollments", helperEnrollmentExchange(opts.Enrollment))
			mux.HandleFunc("POST /v1/helper-identity-renewals", helperIdentityRenew(opts.Enrollment))
			if opts.EdgeControlAdmin != nil {
				mux.HandleFunc("POST /v1/helper-trust/revocations", helperRevocations(opts.EdgeControlAdmin, opts.Enrollment))
			}
			if opts.Machines != nil {
				mux.Handle("POST /v1/machine-installation-failures", helperInstallationFailure(opts.Enrollment, opts.Machines))
				mux.Handle("POST /v1/helper-runtime-policies/resolve", helperRuntimePolicyResolve(opts.Enrollment, opts.Machines))
			}
			if opts.Auth != nil {
				mux.Handle("POST /v1/environments/{environment_id}/helper-enrollments", requireAuth(opts.Auth, requireCSRF(opts.Auth, helperEnrollmentIssue(opts.Enrollment))))
				mux.Handle("POST /v1/environments/{environment_id}/helpers/{helper_id}/replace", requireAuth(opts.Auth, requireCSRF(opts.Auth, helperReplacement(opts.Enrollment))))
			}
		}
		if opts.ReleaseAuthority != nil {
			mux.Handle("GET /v1/admin/release-authority/bundles", operatorRoute(opts, releaseAuthorityBundles(opts.ReleaseAuthority)))
			mux.Handle("POST /v1/admin/release-authority/bundles", operatorRoute(opts, releaseAuthorityBundleImport(opts.ReleaseAuthority)))
			mux.Handle("GET /v1/admin/release-authority/requests", operatorRoute(opts, releaseAuthorityRequests(opts.ReleaseAuthority)))
			mux.Handle("POST /v1/admin/release-authority/requests", operatorRoute(opts, releaseAuthorityRequestCreate(opts.ReleaseAuthority)))
		}
		if opts.ConfigCredentials != nil {
			mux.HandleFunc("POST /v1/config/credentials", configCredentialIssue(opts.ConfigCredentials))
		}
		if opts.ConfigLeases != nil {
			mux.HandleFunc("POST /v1/config/leases/acquire", configLeaseAcquire(opts.ConfigLeases))
			mux.HandleFunc("POST /v1/config/leases/renew", configLeaseRenew(opts.ConfigLeases))
			mux.HandleFunc("POST /v1/config/leases/release", configLeaseRelease(opts.ConfigLeases))
		}
		if opts.ConfigStatuses != nil {
			mux.HandleFunc("POST /v1/config/status", configStatusRecord(opts.ConfigStatuses, opts.Logger))
		}
		if opts.ConfigRepositoryAccess != nil {
			mux.HandleFunc("POST /v1/config/repository-access", configRepositoryAccessIssue(opts.ConfigRepositoryAccess))
		}
		if opts.ConfigRuntime != nil {
			mux.HandleFunc("POST /v1/config/runtime", configRuntimeGet(opts.ConfigRuntime))
		}
		if opts.ConfigConflicts != nil {
			mux.HandleFunc("POST /v1/config/conflict-resolutions/pending", configConflictPending(opts.ConfigConflicts))
			mux.HandleFunc("POST /v1/config/conflict-resolutions/acknowledge", configConflictAcknowledge(opts.ConfigConflicts))
		}
		if opts.Auth != nil {
			registerAuthRoutes(mux, opts)
			if opts.PeerIdentity != nil && opts.DeviceAuth != nil {
				peerRead := func(next http.Handler) http.Handler {
					return requireBearerAuth(opts.DeviceAuth, requireScope("machines:read", next))
				}
				peerWrite := func(next http.Handler) http.Handler {
					return requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", next))
				}
				mux.Handle("GET /v1/e2ee/root", peerRead(e2eeRootGet(opts.PeerIdentity)))
				mux.Handle("GET /v1/peer-keys", peerRead(transportKeysGet(opts.PeerIdentity)))
				mux.Handle("GET /v1/e2ee/pending-endpoints", peerRead(pendingEndpoints(opts.PeerIdentity)))
				mux.Handle("POST /v1/e2ee/endpoint-requests", peerWrite(cliEndpointRequest(opts.PeerIdentity)))
				mux.Handle("POST /v1/e2ee/bootstrap", peerWrite(e2eeBootstrap(opts.PeerIdentity)))
				mux.Handle("GET /v1/endpoints/{endpoint_id}/certificates/{generation}", peerRead(endpointCertificateGet(opts.PeerIdentity)))
				mux.Handle("PUT /v1/endpoints/{endpoint_id}/certificates/{generation}", peerWrite(endpointCertificateRegister(opts.PeerIdentity)))
				mux.Handle("DELETE /v1/endpoints/{endpoint_id}/certificates/{generation}", peerWrite(endpointCertificateRevoke(opts.PeerIdentity)))
			}
			if opts.PeerNetwork != nil && opts.DeviceAuth != nil {
				peerNetworkAuth := func(next http.Handler) http.Handler {
					return requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", next))
				}
				mux.Handle("POST /v1/peer-network/register", peerNetworkAuth(peerNetworkRegister(opts.PeerNetwork)))
				mux.Handle("POST /v1/peer-network/config", peerNetworkAuth(peerNetworkConfig(opts.PeerNetwork)))
			}
			if opts.ManagedSSH != nil && opts.DeviceAuth != nil {
				sshRead := func(next http.Handler) http.Handler {
					return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:read", next))
				}
				sshWrite := func(next http.Handler) http.Handler {
					return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:connect", requireCSRF(opts.Auth, next)))
				}
				cliWrite := func(next http.Handler) http.Handler {
					return requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", next))
				}
				mux.Handle("PUT /v1/ssh/client-keys/{fingerprint}", cliWrite(managedSSHClientKeyPut(opts.ManagedSSH)))
				mux.Handle("DELETE /v1/ssh/client-keys/{fingerprint}", cliWrite(managedSSHClientKeyDelete(opts.ManagedSSH)))
				mux.Handle("PUT /v1/machines/{machine_id}/ssh-target", sshWrite(managedSSHTargetPut(opts.ManagedSSH)))
				mux.Handle("GET /v1/machines/{machine_id}/ssh-target", sshRead(managedSSHTargetGet(opts.ManagedSSH)))
				mux.Handle("GET /v1/machines/{machine_id}/ssh-host-keys", sshRead(managedSSHHostKeysGet(opts.ManagedSSH)))
				mux.Handle("POST /v1/machines/{machine_id}/ssh-host-keys/{set_id}/promote", sshWrite(managedSSHHostKeysPromote(opts.ManagedSSH)))
			}
			if opts.DiagnosticUploads != nil && opts.DeviceAuth != nil {
				diagnosticAuth := func(next http.Handler) http.Handler {
					return requireBearerAuth(opts.DeviceAuth, requireScope("diagnostics:upload", next))
				}
				mux.Handle("POST /v1/diagnostic-upload-intents", diagnosticAuth(diagnosticUploadIntentCreate(opts.DiagnosticUploads)))
				mux.Handle("POST /v1/diagnostic-upload-intents/{intent_id}/complete", diagnosticAuth(diagnosticUploadIntentComplete(opts.DiagnosticUploads)))
			}
			if opts.Routes != nil {
				mux.Handle("POST /v1/environments/{environment_id}/routes", requireAuth(opts.Auth, requireCSRF(opts.Auth, routeIntentCreate(opts.Routes))))
				mux.Handle("PATCH /v1/routes/{route_id}", requireAuth(opts.Auth, requireCSRF(opts.Auth, routeIntentTransition(opts.Routes))))
			}
			if opts.ConfigAssignments != nil {
				configAuth := func(scope string, next http.Handler) http.Handler {
					next = requireWorkspaceSelection(opts.Workspaces, next)
					if opts.DeviceAuth != nil {
						return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope(scope, next))
					}
					return requireAuth(opts.Auth, next)
				}
				if opts.ConfigRepositoryAccess != nil && opts.DeviceAuth != nil {
					mux.Handle("POST /v1/config-repositories/{repository_id}/read-access", requireBearerAuth(opts.DeviceAuth, requireScope("machines:read", configRepositoryReadAccess(opts.ConfigRepositoryAccess))))
				}
				mux.Handle("GET /v1/config-repositories", configAuth("machines:read", configRepositories(opts.ConfigAssignments)))
				mux.Handle("GET /v1/config-repositories/{repository_id}", configAuth("machines:read", configRepositoryGet(opts.ConfigAssignments)))
				mux.Handle("GET /v1/config-repositories/candidates", configAuth("machines:read", configRepositoryCandidates(opts.ConfigAssignments)))
				mux.Handle("POST /v1/config-repositories", configAuth("machines:connect", requireCSRF(opts.Auth, configRepositoryConnect(opts.ConfigAssignments))))
				mux.Handle("DELETE /v1/config-repositories/{repository_id}", configAuth("machines:connect", requireCSRF(opts.Auth, configRepositoryDisconnect(opts.ConfigAssignments))))
				mux.Handle("GET /v1/machines/{machine_id}/config-assignment", configAuth("machines:read", configAssignmentGet(opts.ConfigAssignments)))
				mux.Handle("PUT /v1/machines/{machine_id}/config-assignment", configAuth("machines:connect", requireCSRF(opts.Auth, configAssignmentSet(opts.ConfigAssignments))))
				mux.Handle("GET /v1/machines/{machine_id}/config-assignment/warning", configAuth("machines:read", configWarning(opts.ConfigAssignments)))
				mux.Handle("POST /v1/machines/{machine_id}/config-assignment/consent", configAuth("machines:connect", requireCSRF(opts.Auth, configConsent(opts.ConfigAssignments))))
				mux.Handle("DELETE /v1/machines/{machine_id}/config-assignment/consent", configAuth("machines:connect", requireCSRF(opts.Auth, configConsentRemove(opts.ConfigAssignments))))
				mux.Handle("DELETE /v1/machines/{machine_id}/config-assignment", configAuth("machines:connect", requireCSRF(opts.Auth, configAssignmentClear(opts.ConfigAssignments))))
				mux.Handle("POST /v1/machines/{machine_id}/config-assignment/approve", configAuth("machines:connect", requireCSRF(opts.Auth, configPullRevisionApprove(opts.ConfigAssignments))))
				mux.Handle("GET /v1/teams/{team_id}/config-default", configAuth("machines:read", configTeamDefault(opts.ConfigAssignments)))
				mux.Handle("PUT /v1/teams/{team_id}/config-default", configAuth("machines:connect", requireCSRF(opts.Auth, configTeamDefault(opts.ConfigAssignments))))
				mux.Handle("DELETE /v1/teams/{team_id}/config-default", configAuth("machines:connect", requireCSRF(opts.Auth, configTeamDefault(opts.ConfigAssignments))))
				mux.Handle("GET /v1/config-sync/team-default-adoption", configAuth("machines:read", configTeamDefaultAdoption(opts.ConfigAssignments)))
				mux.Handle("PUT /v1/config-sync/team-default-adoption", configAuth("machines:connect", requireCSRF(opts.Auth, configTeamDefaultAdoption(opts.ConfigAssignments))))
				mux.Handle("DELETE /v1/config-sync/team-default-adoption", configAuth("machines:connect", requireCSRF(opts.Auth, configTeamDefaultAdoption(opts.ConfigAssignments))))
			}
			if opts.Teams != nil {
				secure := func(scope string, next http.Handler) http.Handler {
					if opts.DeviceAuth != nil {
						return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope(scope, next))
					}
					return requireAuth(opts.Auth, next)
				}
				registerTeamRoutes(mux, opts.Teams, func(h http.Handler) http.Handler { return secure("machines:read", h) }, func(h http.Handler) http.Handler { return secure("machines:connect", requireCSRF(opts.Auth, h)) })

				if opts.TeamInbox != nil {
					registerTeamInboxRoutes(mux, opts.TeamInbox, func(h http.Handler) http.Handler { return secure("machines:read", h) }, func(h http.Handler) http.Handler { return secure("machines:connect", requireCSRF(opts.Auth, h)) })
				}
			}
			if opts.EnvironmentVariables != nil {
				environmentAuth := func(scope string, next http.Handler) http.Handler {
					var secured http.Handler
					if opts.DeviceAuth != nil {
						secured = requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope(scope, next))
					} else {
						secured = requireAuth(opts.Auth, next)
					}
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						noStore(w)
						secured.ServeHTTP(w, r)
					})
				}
				environmentWrite := func(next http.Handler) http.Handler {
					return environmentAuth("machines:connect", requireCSRF(opts.Auth, next))
				}
				environmentRead := func(next http.Handler) http.Handler {
					return environmentAuth("machines:read", next)
				}
				mux.Handle("GET /v1/environment-vault", environmentRead(environmentPasswordVaultGet(opts.EnvironmentVariables)))
				mux.Handle("PUT /v1/environment-vault", environmentWrite(environmentPasswordVaultPut(opts.EnvironmentVariables)))
				registerEnvironmentVaultScopeRoutes(mux, opts.EnvironmentVariables, environmentRead, environmentWrite)
				registerEnvironmentVaultHostRoutes(mux, opts.EnvironmentVariables, opts.RuntimeIdentity)
				registerEnvironmentVaultLayerRoutes(mux, opts.EnvironmentVariables, opts.RuntimeIdentity, environmentRead, environmentWrite)
				registerEnvironmentVaultRecordRoutes(mux, opts.EnvironmentVariables, opts.RuntimeIdentity, environmentRead, environmentWrite)
			}
		}
		if opts.Machines != nil {
			mux.HandleFunc("POST /v1/machine-control-renewals", machineControlRenew(opts.Machines))
			userMachineAuth := func(scope string, next http.Handler) http.Handler {
				next = requireWorkspaceSelection(opts.Workspaces, next)
				if opts.DeviceAuth != nil {
					return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope(scope, next))
				}
				return requireAuth(opts.Auth, next)
			}
			mux.HandleFunc("POST /v1/machines/pairings", userMachinePairings(opts.Machines))
			mux.HandleFunc("POST /v1/machines/pairings/reset", userMachinePairingReset(opts.Machines))
			if opts.DeviceAuth != nil {
				if opts.NativePrivateAccess != nil {
					mux.Handle("POST /v1/native-private-access/grants", requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", nativePrivateGrant(opts.NativePrivateAccess))))
				}
				mux.Handle("POST /v1/machines/setup", requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", machineSetup(opts.Machines))))
				mux.Handle("POST /v1/machines/{machine_id}/machine-installations", requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", authenticatedMachineInstallation(opts.Machines))))
				mux.Handle("POST /v1/machines/{machine_id}/control-credentials", requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", machineControlIssue(opts.Machines))))
			}
			mux.Handle("POST /v1/machine-enrollments", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineEnrollmentStart(opts.Machines))))
			mux.Handle("GET /v1/machine-enrollments/{enrollment_id}", userMachineAuth("machines:read", userMachineEnrollmentStatus(opts.Machines)))
			mux.Handle("POST /v1/machine-enrollments/{enrollment_id}/cancel", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineEnrollmentCancel(opts.Machines))))
			mux.Handle("POST /v1/machine-enrollments/{enrollment_id}/retry", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineEnrollmentRetry(opts.Machines))))
			mux.Handle("GET /v1/machines/overview", userMachineAuth("machines:read", userMachineOverview(opts.Machines)))
			mux.Handle("GET /v1/machines/update-summary", userMachineAuth("machines:read", userMachineUpdateSummary(opts.Machines)))
			mux.Handle("GET /v1/transfer-destination-default", userMachineAuth("machines:read", transferDestinationDefault(opts.Machines)))
			mux.Handle("PUT /v1/transfer-destination-default", userMachineAuth("machines:connect", transferDestinationDefault(opts.Machines)))
			mux.Handle("DELETE /v1/transfer-destination-default", userMachineAuth("machines:connect", transferDestinationDefault(opts.Machines)))
			mux.Handle("GET /v1/terminal-sessions/{session_id}/transfer-destination", userMachineAuth("machines:read", terminalSessionTransferDestination(opts.Machines)))
			mux.Handle("PUT /v1/terminal-sessions/{session_id}/transfer-destination", userMachineAuth("machines:connect", terminalSessionTransferDestination(opts.Machines)))
			mux.Handle("DELETE /v1/terminal-sessions/{session_id}/transfer-destination", userMachineAuth("machines:connect", terminalSessionTransferDestination(opts.Machines)))
			mux.Handle("GET /v1/machines/{machine_id}", userMachineAuth("machines:read", userMachineGet(opts.Machines)))
			if opts.MachineServices != nil {
				mux.Handle("GET /v1/machines/{machine_id}/private-access-policy", userMachineAuth("machines:read", machineServicePolicy(opts.MachineServices)))
				mux.Handle("PUT /v1/machines/{machine_id}/private-access-policy", userMachineAuth("machines:connect", requireCSRF(opts.Auth, machineServicePolicy(opts.MachineServices))))
				mux.Handle("GET /v1/machine-services", userMachineAuth("machines:read", machineServiceList(opts.MachineServices)))
				if opts.NativePrivateAccess != nil {
					mux.HandleFunc("POST /v1/runtime-machine-services/authorize", runtimeMachineServiceAuthorize(opts.RuntimeIdentity, opts.NativePrivateAccess))
				}
			}
			mux.Handle("PATCH /v1/machines/{machine_id}", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineRename(opts.Machines))))
			mux.Handle("GET /v1/machines/{machine_id}/update-status", userMachineAuth("machines:read", userMachineUpdateStatus(opts.Machines)))
			mux.Handle("GET /v1/machines/{machine_id}/maintenance-approvals", userMachineAuth("machines:read", userMachineMaintenanceApprovalsList(opts.Machines)))
			mux.Handle("POST /v1/machines/{machine_id}/maintenance-approvals", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineMaintenanceApprovalRequest(opts.Machines))))
			mux.Handle("POST /v1/machines/{machine_id}/maintenance-approvals/{approval_id}/approve", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineMaintenanceApprovalDecision(opts.Machines, "approved"))))
			mux.Handle("POST /v1/machines/{machine_id}/maintenance-approvals/{approval_id}/reject", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineMaintenanceApprovalDecision(opts.Machines, "rejected"))))
			mux.Handle("PUT /v1/machines/{machine_id}/availability-policy", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineAvailabilityPolicy(opts.Machines))))
			mux.Handle("PUT /v1/machines/{machine_id}/capabilities", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineCapabilities(opts.Machines))))
			if opts.DeviceAuth != nil {
				mux.Handle("POST /v1/machines/{machine_id}/connection-descriptor", requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", requireWorkspaceSelection(opts.Workspaces, userMachineConnectionDescriptor(opts.Machines)))))
				mux.Handle("POST /v1/terminal-sessions/{session_id}/connection-descriptor", requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", requireWorkspaceSelection(opts.Workspaces, sharedTerminalConnectionDescriptor(opts.Machines)))))
				terminalRead := func(h http.Handler) http.Handler {
					return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:read", requireWorkspaceSelection(opts.Workspaces, h)))
				}
				terminalWrite := func(h http.Handler) http.Handler {
					return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:connect", requireCSRF(opts.Auth, requireWorkspaceSelection(opts.Workspaces, h))))
				}
				mux.Handle("GET /v1/terminal-sessions", terminalRead(terminalSessionCatalog(opts.Machines)))
				mux.Handle("GET /v1/terminal-sessions/{session_id}/sharing", terminalRead(terminalSessionSharing(opts.Machines)))
				mux.Handle("POST /v1/terminal-sessions/{session_id}/sharing", terminalWrite(terminalSessionGrant(opts.Machines)))
				mux.Handle("DELETE /v1/terminal-sessions/{session_id}/sharing", terminalWrite(terminalSessionEndSharing(opts.Machines)))
				mux.Handle("POST /v1/terminal-sessions/{session_id}/participants/{account_id}/remove", terminalWrite(terminalSessionRemoveParticipant(opts.Machines)))
				mux.Handle("POST /v1/machines/{machine_id}/exec-descriptor", requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", requireWorkspaceSelection(opts.Workspaces, userMachineExecDescriptor(opts.Machines)))))
				mux.Handle("POST /v1/machines/{machine_id}/ssh-descriptor", requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", requireWorkspaceSelection(opts.Workspaces, userMachineSSHDescriptor(opts.Machines)))))
				mux.Handle("POST /v1/machines/{machine_id}/file-transfer-descriptor", requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", requireWorkspaceSelection(opts.Workspaces, userMachineFileTransferDescriptor(opts.Machines)))))
				mux.Handle("GET /v1/machines/{machine_id}/connection-readiness", requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", requireWorkspaceSelection(opts.Workspaces, userMachineConnectionReadiness(opts.Machines)))))
				mux.Handle("GET /v1/terminal-sessions/{session_id}/transfer-destinations", requireBearerAuth(opts.DeviceAuth, requireScope("machines:read", requireWorkspaceSelection(opts.Workspaces, terminalSessionTransferDestinations(opts.Machines)))))
			}
			mux.Handle("GET /v1/terminal-sessions/{session_id}/browser-access-policy", requireAuth(opts.Auth, terminalBrowserAccessPolicyGet(opts.Machines)))
			mux.Handle("PUT /v1/terminal-sessions/{session_id}/browser-access-policy", requireAuth(opts.Auth, requireCSRF(opts.Auth, terminalBrowserAccessPolicyPut(opts.Machines))))
			mux.Handle("GET /v1/terminal-sessions/{session_id}/code-access", requireAuth(opts.Auth, terminalCodeAccessGet(opts.Machines)))
			mux.Handle("PUT /v1/terminal-sessions/{session_id}/code-access", requireAuth(opts.Auth, requireCSRF(opts.Auth, terminalCodeAccessPut(opts.Machines))))
			mux.HandleFunc("GET /v1/terminal-sessions/{session_id}/browser-session", terminalBrowserSession(opts.Machines, opts.Auth))
			mux.HandleFunc("POST /v1/terminal-sessions/{session_id}/code-join", terminalCodeJoin(opts.Machines, opts.Auth))
			if opts.BrowserTerminal != nil {
				mux.HandleFunc("POST /v1/terminal-sessions/{session_id}/guest-browser-ticket", terminalGuestBrowserTicket(opts.Machines, opts.Auth, opts.BrowserTerminal, opts.PeerIdentity))
				mux.Handle("POST /v1/terminal-sessions/{session_id}/browser-ticket", requireAuth(opts.Auth, requireCSRF(opts.Auth, terminalBrowserTicket(opts.BrowserTerminal, opts.PeerIdentity))))
			}
			mux.Handle("GET /v1/machines/{machine_id}/terminal-sessions", userMachineAuth("machines:read", userMachineTerminalSessionsList(opts.Machines)))
			mux.Handle("POST /v1/machines/{machine_id}/terminal-sessions", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineTerminalSessionsCreate(opts.Machines))))
			mux.Handle("PATCH /v1/machines/{machine_id}/terminal-sessions/{session_id}", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineTerminalSessionsRename(opts.Machines))))
			mux.Handle("POST /v1/machines/{machine_id}/terminal-sessions/{session_id}/close", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineTerminalSessionsClose(opts.Machines))))
			mux.Handle("DELETE /v1/machines/{machine_id}/terminal-sessions/{session_id}", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineTerminalSessionsDelete(opts.Machines))))
			mux.HandleFunc("POST /v1/machines/pairings/installation", userMachineInstallationConsume(opts.Machines))
			mux.Handle("POST /v1/machines/{machine_id}/disconnect", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineDisconnect(opts.Machines))))
			mux.Handle("DELETE /v1/machines/{machine_id}", userMachineAuth("machines:connect", requireCSRF(opts.Auth, userMachineDelete(opts.Machines))))
			if opts.DeviceAuth != nil {
				mux.Handle("GET /v1/machines", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:read", requireWorkspaceSelection(opts.Workspaces, machinesList(opts.Machines)))))
			} else {
				mux.Handle("GET /v1/machines", requireAuth(opts.Auth, requireWorkspaceSelection(opts.Workspaces, machinesList(opts.Machines))))
			}
		}
		if opts.ManagedSSH != nil && opts.RuntimeIdentity != nil {
			mux.HandleFunc("PUT /v1/machines/{machine_id}/ssh-host-keys", managedSSHHostKeysObserve(opts.ManagedSSH, opts.RuntimeIdentity))
			mux.HandleFunc("POST /v1/machines/{machine_id}/ssh-authorized-keys", managedSSHAuthorizedKeys(opts.ManagedSSH, opts.RuntimeIdentity))
		}
		if opts.PeerIdentity != nil && opts.RuntimeIdentity != nil {
			mux.HandleFunc("POST /v1/machine-peer-identity", machineEndpointRequest(opts.PeerIdentity, opts.RuntimeIdentity))
			mux.HandleFunc("POST /v1/machine-peer-identity/status", machineEndpointStatus(opts.PeerIdentity, opts.RuntimeIdentity))
		}
		if opts.Machines != nil && opts.RuntimeIdentity != nil {
			mux.HandleFunc("POST /v1/machine-control-credentials", machineControlInitial(opts.Machines, opts.RuntimeIdentity))
		}
		if opts.PeerNetwork != nil && opts.RuntimeIdentity != nil {
			mux.HandleFunc("POST /v1/machine-peer-network/register", machinePeerNetworkRegister(opts.PeerNetwork, opts.RuntimeIdentity))
			mux.HandleFunc("POST /v1/machine-peer-network/config", machinePeerNetworkConfig(opts.PeerNetwork, opts.RuntimeIdentity))
		}
		if opts.WorkspaceBilling != nil {
			mux.Handle("GET /v1/billing/workspaces/personal", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("account:read", workspaceBillingOverview(opts.WorkspaceBilling))))
			mux.Handle("GET /v1/billing/workspaces/personal/products", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("account:read", workspaceBillingProducts(opts.WorkspaceBilling))))
			mux.Handle("POST /v1/billing/workspaces/personal/checkout", requireAuth(opts.Auth, requireCSRF(opts.Auth, workspaceBillingMutation(opts.WorkspaceBilling, opts.Config.HTTP.AllowedOrigins, false))))
			mux.Handle("POST /v1/billing/workspaces/personal/portal", requireAuth(opts.Auth, requireCSRF(opts.Auth, workspaceBillingMutation(opts.WorkspaceBilling, opts.Config.HTTP.AllowedOrigins, true))))
			mux.Handle("GET /v1/billing/workspaces/teams/{team_id}", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("account:read", workspaceBillingOverview(opts.WorkspaceBilling))))
			mux.Handle("GET /v1/billing/workspaces/teams/{team_id}/products", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("account:read", workspaceBillingProducts(opts.WorkspaceBilling))))
			mux.Handle("POST /v1/billing/workspaces/teams/{team_id}/checkout", requireAuth(opts.Auth, requireCSRF(opts.Auth, workspaceBillingMutation(opts.WorkspaceBilling, opts.Config.HTTP.AllowedOrigins, false))))
			mux.Handle("POST /v1/billing/workspaces/teams/{team_id}/portal", requireAuth(opts.Auth, requireCSRF(opts.Auth, workspaceBillingMutation(opts.WorkspaceBilling, opts.Config.HTTP.AllowedOrigins, true))))
			mux.HandleFunc("POST /v1/webhooks/stripe", stripeWorkspaceWebhook(opts.WorkspaceBilling, opts.Config.Secrets.StripeWebhookSecret))
		}
		if opts.MeteringRepo != nil {
			mux.HandleFunc("POST /v1/runtime-observations", runtimeObservation(opts.MeteringRepo, opts.RuntimeIdentity, opts.Config.ConfigSync.SummaryLimit, opts.Machines, opts.EnvironmentVariables, opts.LazyActivation, opts.MachineServices))
		}
		mux.HandleFunc("/", notFound)
		handler = mux
	}
	handler = clientUpdatePolicyGuard(opts.ClientUpdatePolicy, time.Now, handler)
	handler = workspaceQueryContext(handler)
	handler = secureHeaders(handler)
	handler = cors(opts.Config.HTTP.AllowedOrigins, handler)
	handler = bodyLimit(opts.Config.HTTP.MaxBodyBytes, handler)
	handler = timeout(opts.Config.HTTP.RequestTimeout, opts.Logger, handler)
	handler = recoverer(opts.Logger, handler)
	if opts.TelemetryHTTP != nil {
		// Observe outside timeout and panic recovery so recorded status/outcome
		// reflects the response the caller actually received.
		handler = opts.TelemetryHTTP.WrapFunc(telemetryRouteFamily, handler)
	}
	handler = accessLog(opts.Logger, handler)
	handler = trustedClientNetwork(opts.Config.HTTP.TrustedProxyCIDRs, handler)
	handler = supportMiddleware(opts.SupportReports, opts.Logger, handler)
	handler = correlationID(handler)
	handler = requestID(handler)
	return handler
}

func telemetryRouteFamily(r *http.Request) string {
	path := r.URL.Path
	switch {
	case path == "/healthz", path == "/readyz", path == "/metrics", path == telemetryDiagnosticsPath:
		return "health"
	case strings.HasPrefix(path, "/v1/edge/"):
		return "edge_control"
	case path == "/install", strings.HasPrefix(path, "/tuf/"), strings.HasPrefix(path, "/helper-releases/"):
		return "release"
	case strings.HasPrefix(path, "/v1/"), strings.HasPrefix(path, "/.well-known/"):
		return "public_api"
	default:
		return "other"
	}
}

func correlationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID := serverdiagnostics.NormalizeRequestID(r.Header.Get("Correlation-Id"))
		if correlationID == "" {
			correlationID = serverdiagnostics.RequestID(r.Context())
		}
		w.Header().Set("Correlation-Id", correlationID)
		next.ServeHTTP(w, r.WithContext(serverdiagnostics.WithCorrelationID(r.Context(), correlationID)))
	})
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{
		"status": "healthy",
	}})
}

func metrics(diagnostics *controlplane.DiagnosticsService, typed *telemetry.Metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			writeError(w, r, http.StatusForbidden, "forbidden", "Metrics are available only from localhost.")
			return
		}
		legacy := serverdiagnostics.MetricsSnapshot()
		if diagnostics != nil {
			durable, err := diagnostics.Metrics(r.Context())
			if err != nil {
				writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "Control-plane diagnostics are unavailable.", err)
				return
			}
			for key, value := range durable {
				legacy[key] = value
			}
		}
		result := make(map[string]any, len(legacy)+1)
		for key, value := range legacy {
			result[key] = value
		}
		if typed != nil {
			result["typed"] = typed.Snapshot()
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: result})
	}
}

func ready(checker ReadinessChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if checker == nil {
			writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "Readiness checks are not configured.")
			return
		}
		if err := checker.Ready(r.Context()); err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "Service dependencies are not ready.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{
			"status": "ready",
		}})
	}
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, "not_found", "The requested endpoint was not found.")
}

func registerAuthRoutes(mux *http.ServeMux, opts Options) {
	if opts.SupportReports != nil {
		mux.Handle("GET /v1/admin/support/{reference}", operatorRoute(opts, supportLookup(opts.SupportReports, true, opts.Config.Reporting.IssueURL)))
		mux.Handle("GET /v1/support/{reference}", requireAnyAuth(opts.Auth, opts.DeviceAuth, supportLookup(opts.SupportReports, false, "")))
	}
	userAuth := func(scope string, next http.Handler) http.Handler {
		next = requireWorkspaceSelection(opts.Workspaces, next)
		if opts.DeviceAuth != nil {
			return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope(scope, next))
		}
		return requireAuth(opts.Auth, next)
	}
	accountRead := func(next http.Handler) http.Handler {
		next = requireWorkspaceSelection(opts.Workspaces, next)
		if opts.DeviceAuth != nil {
			return requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("account:read", next))
		}
		return requireAuth(opts.Auth, next)
	}
	if opts.BrowserAccess != nil {
		viewerPolicyPath := "/v1/browser-access/policies/{resource_kind}/{resource_id}/{route_id}"
		mux.Handle("GET "+viewerPolicyPath, requireAuth(opts.Auth, http.HandlerFunc(opts.BrowserAccess.ViewerPolicyGet)))
		mux.Handle("PUT "+viewerPolicyPath, requireAuth(opts.Auth, requireCSRF(opts.Auth, http.HandlerFunc(opts.BrowserAccess.ViewerPolicyPut))))
		mux.Handle("POST /v1/browser-access/machine-credentials", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:connect", requireCSRF(opts.Auth, http.HandlerFunc(opts.BrowserAccess.MachineCredential)))))
		mux.Handle("POST /v1/browser-access/issue", requireAuth(opts.Auth, requireCSRF(opts.Auth, http.HandlerFunc(opts.BrowserAccess.Issue))))
		mux.HandleFunc("POST /v1/edge/browser-access/{operation}", opts.BrowserAccess.EdgeRequest)
		mux.HandleFunc("POST /v1/browser-access/ingress/authorize", opts.BrowserAccess.MachineRequest)
	}
	if opts.Inspector != nil {
		mux.Handle("POST /v1/inspector/targets", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:connect", requireCSRF(opts.Auth, http.HandlerFunc(opts.Inspector.Targets)))))
		mux.Handle("POST /v1/inspector/access", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:connect", requireCSRF(opts.Auth, http.HandlerFunc(opts.Inspector.ResolveAccess)))))
		mux.Handle("POST /v1/inspector/credentials", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:connect", requireCSRF(opts.Auth, http.HandlerFunc(opts.Inspector.IssueCredential)))))
		mux.Handle("DELETE /v1/inspector/credentials/{credential_id}", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("machines:connect", requireCSRF(opts.Auth, http.HandlerFunc(opts.Inspector.RevokeCredential)))))
		mux.HandleFunc("POST /v1/inspector/authorize", opts.Inspector.Authorize)
	}
	mux.HandleFunc("GET /v1/auth/workos/state", workOSState(opts.Auth))
	mux.HandleFunc("POST /v1/auth/workos/callback", workOSCallback(opts.Auth))
	mux.HandleFunc("POST /v1/auth/logout", logout(opts.Auth))
	mux.Handle("GET /v1/auth/csrf", requireAuth(opts.Auth, csrf(opts.Auth)))
	meHandler := requireAuth(opts.Auth, me(opts.Auth))
	if opts.DeviceAuth != nil {
		meHandler = requireAnyAuth(opts.Auth, opts.DeviceAuth, me(opts.Auth))
	}
	mux.Handle("GET /v1/me", meHandler)
	if opts.Favorites != nil {
		mux.Handle("GET /v1/favorites", accountRead(favoritesList(opts.Favorites)))
		mux.Handle("PUT /v1/favorites", accountRead(requireCSRF(opts.Auth, favoriteSet(opts.Favorites))))
	}
	if opts.ConfigStatuses != nil {
		mux.Handle("GET /v1/config-sync/status", accountRead(configSyncStatus(opts.ConfigStatuses)))
		mux.Handle("GET /v1/config-sync/environments/{environment_id}/status", accountRead(configSyncEnvironmentStatus(opts.ConfigStatuses)))
	}
	if opts.ConfigConflicts != nil {
		if opts.Machines != nil {
			mux.Handle("POST /v1/config-sync/environments/{environment_id}/conflicts/{conflict_revision}/connection", userAuth("machines:connect", requireCSRF(opts.Auth, configCompareConnection(opts.ConfigConflicts, opts.Machines))))
		}
		mux.Handle("POST /v1/config-sync/environments/{environment_id}/conflict-resolutions", userAuth("machines:connect", requireCSRF(opts.Auth, configConflictRequest(opts.ConfigConflicts))))
		mux.Handle("POST /v1/config-sync/environments/{environment_id}/force", userAuth("machines:connect", requireCSRF(opts.Auth, configForceRequest(opts.ConfigConflicts))))
	}
	if opts.DeviceAuth != nil {
		mux.HandleFunc("POST /v1/auth/device/authorize", deviceAuthorize(opts.DeviceAuth, resolvedRequestNetwork))
		mux.HandleFunc("POST /v1/auth/device/token", deviceToken(opts.DeviceAuth, resolvedRequestNetwork))
		mux.HandleFunc("POST /v1/auth/device/cancel", deviceCancel(opts.DeviceAuth, resolvedRequestNetwork))
		mux.Handle("GET /v1/auth/device/requests/{user_code}", requireAuth(opts.Auth, deviceRequest(opts.DeviceAuth, opts.Config.HTTP.PublicBaseURL)))
		mux.Handle("POST /v1/auth/device/requests/{user_code}/approve", requireAuth(opts.Auth, requireCSRF(opts.Auth, deviceDecision(opts.DeviceAuth, opts.Config.HTTP.PublicBaseURL, true))))
		mux.Handle("POST /v1/auth/device/requests/{user_code}/deny", requireAuth(opts.Auth, requireCSRF(opts.Auth, deviceDecision(opts.DeviceAuth, opts.Config.HTTP.PublicBaseURL, false))))
		mux.HandleFunc("POST /v1/auth/token/refresh", tokenRefresh(opts.DeviceAuth))
		mux.HandleFunc("POST /v1/auth/token/revoke", tokenRevoke(opts.DeviceAuth))
		mux.Handle("GET /v1/auth/cli-client-sessions", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireScope("account:read", cliClientSessionsList(opts.DeviceAuth))))
		mux.Handle("DELETE /v1/auth/cli-client-sessions/{cli_client_session_id}", requireAnyAuth(opts.Auth, opts.DeviceAuth, requireCSRF(opts.Auth, requireScope("clients:revoke", cliClientSessionDelete(opts.DeviceAuth)))))
	}
	if opts.EdgeControlAdmin != nil {
		mux.Handle("POST /v1/admin/edge/usage-keys", operatorRoute(opts, adminProvisionUsageKey(opts.EdgeControlAdmin)))
		mux.Handle("POST /v1/admin/edge/usage-keys/{key_id}/revoke", operatorRoute(opts, adminRevokeUsageKey(opts.EdgeControlAdmin)))
		mux.Handle("POST /v1/admin/mint/signing-keys/{key_id}/revoke", operatorRoute(opts, adminRevokeSigningKey(opts.EdgeControlAdmin)))
	}
	if opts.OperationRecovery != nil {
		mux.Handle("POST /v1/admin/control-operations/{operation_id}/recover", operatorRoute(opts, adminRecoverControlOperation(opts.OperationRecovery)))
	}
	if opts.WorkspaceBillingRecovery != nil {
		mux.Handle("POST /v1/admin/billing/workspace-operations/{operation_id}/retire", operatorRoute(opts, adminRetireWorkspaceBillingOperation(opts.WorkspaceBillingRecovery)))
		mux.Handle("POST /v1/admin/billing/workspace-operations/{operation_id}/reconcile-checkout", operatorRoute(opts, adminReconcileWorkspaceCheckout(opts.WorkspaceBillingRecovery)))
		mux.Handle("POST /v1/admin/billing/workspace-operations/{operation_id}/supersede-portal", operatorRoute(opts, adminSupersedeWorkspacePortal(opts.WorkspaceBillingRecovery)))
	}
	if opts.GitHub != nil {
		if opts.DeviceAuth != nil {
			nativeGitHubAuth := func(handler http.Handler) http.Handler {
				return requireBearerAuth(opts.DeviceAuth, requireScope("machines:connect", handler))
			}
			mux.Handle("POST /v1/github/native-links", nativeGitHubAuth(githubNativeStart(opts.GitHub)))
			mux.Handle("GET /v1/github/native-links/{link_id}", nativeGitHubAuth(githubNativeStatus(opts.GitHub, false)))
			mux.Handle("DELETE /v1/github/native-links/{link_id}", nativeGitHubAuth(githubNativeStatus(opts.GitHub, true)))
			mux.HandleFunc("GET /v1/github/native-links/{link_id}/launch", githubNativeLaunch(opts.GitHub))
			mux.HandleFunc("POST /v1/github/native-links/{link_id}/launch", githubNativeLaunch(opts.GitHub))
		}
		mux.Handle("GET /v1/github/status", requireAuth(opts.Auth, githubStatus(opts.GitHub)))
		mux.Handle("GET /v1/github/repositories", userAuth("machines:read", githubRepositories(opts.GitHub)))
		mux.Handle("POST /v1/github/oauth/start", requireAuth(opts.Auth, requireCSRF(opts.Auth, githubOAuthStart(opts.Auth, opts.GitHub))))
		mux.Handle("GET /v1/github/oauth/callback", githubCallbackDispatch(opts.GitHub, requireAuth(opts.Auth, githubOAuthBrowserCallback(opts.Auth, opts.GitHub))))
		mux.Handle("POST /v1/github/oauth/callback", requireAuth(opts.Auth, requireCSRF(opts.Auth, githubOAuthCallback(opts.Auth, opts.GitHub))))
		mux.Handle("POST /v1/github/config-repositories/provision", requireAuth(opts.Auth, requireCSRF(opts.Auth, githubProvisionConfigRepo(opts.GitHub))))
	}
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := serverdiagnostics.NormalizeRequestID(r.Header.Get("Request-Id"))
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set("Request-Id", requestID)
		next.ServeHTTP(w, r.WithContext(serverdiagnostics.WithRequestID(r.Context(), requestID)))
	})
}

func accessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		serverdiagnostics.LoggerWithRequest(r.Context(), logger).Info("http_request",
			"support_reference", supportReference(r.Context()),
			"method", r.Method,
			"path", safeLogPath(r.URL.Path),
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
