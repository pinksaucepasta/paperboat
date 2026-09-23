package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/operator"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/auth"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/config"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/node"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/observability"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/operation"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
	edgeruntime "github.com/pinksaucepasta/paperboat-tunnel/internal/runtime"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/store"
	edgetelemetry "github.com/pinksaucepasta/paperboat-tunnel/internal/telemetry"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/tlscert"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/usage"
)

var version = "development"

func main() {
	reporter, err := reporting.New("paperboat-tunnel")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := execute(reporter, os.Args[1:])
	reporter.Close()
	os.Exit(code)
}

type serviceFailure struct{ error }

func execute(reporter *reporting.Reporter, args []string) (code int) {
	started := time.Now()
	defer func() {
		if recover() != nil {
			reference := reportUnexpected(reporter, "panic")
			reporter.Observe(context.Background(), "service_lifecycle", "failed", "internal", reference, time.Since(started))
			code = 2
		}
	}()
	if err := run(args, reporter); err != nil {
		var failure serviceFailure
		if errors.As(err, &failure) {
			reference := reportUnexpected(reporter, "service_run")
			reporter.Observe(context.Background(), "service_lifecycle", "failed", "internal", reference, time.Since(started))
		} else {
			reporter.Observe(context.Background(), "service_lifecycle", "rejected", "invalid", "", time.Since(started))
			fmt.Fprintln(os.Stderr, err)
		}
		return 1
	}
	reporter.Observe(context.Background(), "service_lifecycle", "success", "shutdown", "", time.Since(started))
	return 0
}

func reportUnexpected(reporter *reporting.Reporter, kind string) string {
	reference := reporting.Reference()
	if reference == "" {
		fmt.Fprintln(os.Stderr, "paperboat-tunnel stopped unexpectedly")
		return ""
	}
	reporter.Capture(reference, kind, 2)
	fmt.Fprintf(os.Stderr, "paperboat-tunnel stopped unexpectedly; support reference %s\n", reference)
	return reference
}

func run(args []string, reporters ...*reporting.Reporter) error {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(os.Stdout, "paperboat-tunnel %s\n", version)
		return nil
	}
	if handled, err := operator.Dispatch(context.Background(), args, "tunnel", os.Stdout, nil); handled {
		return err
	}
	if generated, handled, err := operator.RuntimeArgs(args); handled {
		if err != nil {
			return err
		}
		args = generated
	}
	cfg, err := config.Parse(args)
	if err != nil {
		return err
	}
	if cfg.DeploymentPath == "" {
		return errors.New("deployment-config is required")
	}
	deployment, err := config.LoadDeployment(cfg.DeploymentPath)
	if err != nil {
		return err
	}
	var reporter *reporting.Reporter
	if len(reporters) > 0 {
		reporter = reporters[0]
	}
	service, err := buildServiceWithReporter(cfg, deployment, reporter)
	if err != nil {
		return err
	}
	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Start(root); err != nil {
		return serviceFailure{err}
	}
	var runtimeErr error
	select {
	case <-root.Done():
	case runtimeErr = <-service.Done():
		stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := service.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return serviceFailure{err}
	}
	if runtimeErr != nil {
		return serviceFailure{runtimeErr}
	}
	return nil
}

func buildService(cfg config.Config, deployment config.Deployment) (*edgeruntime.Service, error) {
	return buildServiceWithCarrier(cfg, deployment, nil)
}

func buildServiceWithReporter(cfg config.Config, deployment config.Deployment, reporter *reporting.Reporter) (*edgeruntime.Service, error) {
	return buildServiceAssembly(cfg, deployment, nil, reporter)
}

// buildServiceWithCarrier is the explicit deployment assembly seam for the
// connector-v1 edge carrier. A supplied carrier is useful for deterministic
// composition tests; the normal path constructs the authenticated carrier
// listener from the dedicated deployment certificate and addresses.
func buildServiceWithCarrier(cfg config.Config, deployment config.Deployment, carrier edgeruntime.Component) (*edgeruntime.Service, error) {
	return buildServiceAssembly(cfg, deployment, carrier, nil)
}
func buildServiceAssembly(cfg config.Config, deployment config.Deployment, carrier edgeruntime.Component, reporter *reporting.Reporter) (*edgeruntime.Service, error) {
	credential, err := readCredential(deployment.ControlCredentialFile)
	if err != nil {
		return nil, fmt.Errorf("load control credential: %w", err)
	}
	trust, err := edgeruntime.LoadTrust(deployment.JWKSFile, deployment.RevocationsFile, deployment.UsageSigningKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load trust: %w", err)
	}
	if trust.UsageEdgeNodeID != cfg.NodeID {
		return nil, fmt.Errorf("load trust: usage signing key is bound to edge node %q, configured node is %q", trust.UsageEdgeNodeID, cfg.NodeID)
	}
	trust.Snapshot.ConfigureRevocationFreshness(3*deployment.ControlInterval, time.Now)
	tlsConfig, err := controlTLS(deployment.ControlCAFile)
	if err != nil {
		return nil, fmt.Errorf("load control TLS: %w", err)
	}
	var controlTrace func(context.Context, string) (string, string, func(string, string))
	if reporter != nil {
		controlTrace = reporter.ControlTrace
	}
	client, err := control.NewHTTPClient(control.HTTPConfig{BaseURL: deployment.ControlURL, Credential: credential, Timeout: deployment.ControlTimeout, TLS: tlsConfig, ControlTrace: controlTrace})
	if err != nil {
		return nil, fmt.Errorf("create control client: %w", err)
	}
	journal, counters, queue, err := restoreState(cfg.StatePath)
	if err != nil {
		return nil, fmt.Errorf("restore edge state: %w", err)
	}
	epoch, err := usage.NewCounterEpoch()
	if err != nil {
		return nil, fmt.Errorf("create counter epoch: %w", err)
	}
	processEpoch, err := usage.NewCounterEpoch()
	if err != nil {
		return nil, fmt.Errorf("create process epoch: %w", err)
	}
	certificateRegistry, err := tlscert.NewRegistry(tlscert.Config{})
	if err != nil {
		return nil, fmt.Errorf("create TLS certificate registry: %w", err)
	}
	distributionAuthenticator, err := control.NewHMACDistributionAuthenticator(credential)
	if err != nil {
		return nil, fmt.Errorf("create TLS certificate distribution authenticator: %w", err)
	}
	certificateReceiver, err := tlscert.NewDistributionReceiver(tlscert.ReceiverConfig{
		Registry: certificateRegistry, NodeID: cfg.NodeID, ProcessEpoch: processEpoch,
		Authenticator: distributionAuthenticator,
	})
	if err != nil {
		return nil, fmt.Errorf("create TLS certificate distribution receiver: %w", err)
	}
	certificateClient, err := control.NewCertificateDistributionClient(client)
	if err != nil {
		_ = certificateReceiver.Close()
		return nil, fmt.Errorf("create TLS certificate distribution client: %w", err)
	}
	onDemandRequester, err := control.NewOnDemandCertificateRequester(certificateClient, cfg.NodeID, processEpoch)
	if err != nil {
		_ = certificateReceiver.Close()
		_ = certificateClient.Close()
		return nil, fmt.Errorf("create TLS on-demand certificate requester: %w", err)
	}
	certificateWorker, err := edgeruntime.NewCertificateDistributionWorker(edgeruntime.CertificateDistributionWorkerConfig{
		Client: certificateClient, Receiver: certificateReceiver, NodeID: cfg.NodeID, ProcessEpoch: processEpoch,
		Interval: deployment.ControlInterval,
	})
	if err != nil {
		_ = certificateReceiver.Close()
		_ = certificateClient.Close()
		return nil, fmt.Errorf("create TLS certificate distribution worker: %w", err)
	}
	certificateSelector, err := edgeruntime.NewCertificateSelector(edgeruntime.CertificateSelectorConfig{
		Source:            certificateRegistry,
		OnDemandRequester: onDemandRequester,
		OnDemandMatcher:   certificateRegistry,
	})
	if err != nil {
		_ = certificateReceiver.Close()
		_ = certificateClient.Close()
		return nil, fmt.Errorf("create TLS certificate broker: %w", err)
	}
	// The broker, receiver, registry, and distribution verifier all own
	// sensitive in-memory state. Until the assembly accepts ownership, every
	// later validation/constructor failure must close and wipe them. The
	// successful return disarms this guard because Assembly/DataPlane then
	// shuts the same components down in its normal lifecycle order.
	certificateOwned := true
	defer func() {
		if !certificateOwned {
			return
		}
		_ = certificateReceiver.Close()
		_ = certificateClient.Close()
	}()
	state := node.New(cfg.NodeID)
	manager, err := node.NewManager(state, deployment.NodeCapacity)
	if err != nil {
		return nil, fmt.Errorf("create node manager: %w", err)
	}
	routes := route.NewRegistry(deployment.PreviewBaseDomain, deployment.RuntimeBaseDomain)
	typedHealth, err := edgetelemetry.NewHealthTracker(time.Now)
	if err != nil {
		return nil, fmt.Errorf("create edge health tracker: %w", err)
	}
	typedEvents, err := edgetelemetry.NewEventLog(1024)
	if err != nil {
		return nil, fmt.Errorf("create edge event log: %w", err)
	}
	eventLogOwned := true
	defer func() {
		if eventLogOwned {
			_ = typedEvents.Close()
		}
	}()
	typedMetrics := edgetelemetry.NewMetrics()
	telemetryLifecycle := newTelemetryLifecycle(typedHealth, typedMetrics, typedEvents)
	telemetryLifecycle.reporter = reporter
	telemetryLifecycle.usageQueue = queue.Stats
	routeTelemetry, err := route.NewCoreTelemetrySink(typedHealth, typedMetrics, typedEvents)
	if err != nil {
		return nil, fmt.Errorf("create route telemetry: %w", err)
	}
	canonicalRoutes, err := route.NewRegistryWithOptions("", "", route.GenerationRegistryOptions{TelemetrySink: routeTelemetry})
	if err != nil {
		return nil, fmt.Errorf("create canonical route registry: %w", err)
	}
	durableAdmissions, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: cfg.NodeID, ProcessEpoch: processEpoch, MaximumAdmissions: 4096})
	if err != nil {
		return nil, fmt.Errorf("create durable carrier admission registry: %w", err)
	}
	snapshotState := func() store.State {
		return store.State{Version: store.CurrentVersion, CounterEpoch: epoch, Operations: journal.Snapshot(), Counters: counters.Snapshot(), PendingUsage: queue.Snapshot()}
	}
	verifier := &auth.Verifier{Issuer: deployment.CredentialIssuer, NodeID: cfg.NodeID, Keys: trust.Snapshot, Revocations: trust.Snapshot, ClockSkew: 30 * time.Second}
	previewWorker, previewHandler, expectedAdmissions, previewRoutes, err := previewCarrierState(cfg.NodeID, processEpoch, deployment, client)
	if err != nil {
		return nil, err
	}
	accessorAdmissions, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: cfg.NodeID, ProcessEpoch: processEpoch, MaximumAdmissions: 4096})
	if err != nil {
		return nil, err
	}
	meter := &usage.Meter{Node: cfg.NodeID, Epoch: epoch, Counters: counters, Queue: queue, KeyID: trust.UsageKeyID, PrivateKey: trust.UsagePrivateKey, Persist: func() error {
		return store.Save(cfg.StatePath, snapshotState())
	}}
	if err := meter.RestoreBaseline(); err != nil {
		return nil, fmt.Errorf("restore usage baseline: %w", err)
	}
	ingressAuthority := &control.IngressAuthority{Client: client, NodeID: cfg.NodeID, ProcessEpoch: processEpoch}
	durableRoutes, err := edgehttp.NewDataCarrierRouteRegistry(edgehttp.DataCarrierRouteRegistryConfig{MaximumRoutes: 4096, Usage: meter, IngressAuthority: ingressAuthority.Resolve})
	if err != nil {
		return nil, fmt.Errorf("create durable carrier route registry: %w", err)
	}
	publicListenHost, _, err := net.SplitHostPort(deployment.PublicHTTPSListenAddress)
	if err != nil {
		return nil, fmt.Errorf("parse public listener address: %w", err)
	}
	if publicListenHost == "" {
		publicListenHost = "0.0.0.0"
	}
	publicTCP, err := edgehttp.NewPublicTCPListeners(edgehttp.PublicTCPListenerConfig{ListenHost: publicListenHost, InfrastructureHostname: deployment.ConnectorAdvertiseHost, Authority: ingressAuthority, Routes: durableRoutes, Interval: deployment.ControlInterval, MaximumListeners: 4096, MaximumConnections: int(deployment.NodeCapacity) * 128})
	if err != nil {
		return nil, fmt.Errorf("configure public TCP listeners: %w", err)
	}
	internalToken, err := newPrivateAccessToken()
	if err != nil {
		return nil, fmt.Errorf("create private access token: %w", err)
	}
	persistence := edgeruntime.Persistence{Path: cfg.StatePath, Restore: func(store.State) error { return nil }, Snapshot: snapshotState}
	carrierEndpoint, err := carrierEndpointFromDeployment(deployment)
	if err != nil {
		return nil, fmt.Errorf("parse carrier endpoints: %w", err)
	}
	if carrierEndpoint == nil {
		return nil, errors.New("parse carrier endpoints: canonical carrier endpoint is required")
	}
	carrierTrust, err := control.NewProcessCarrierServerTrust(cfg.NodeID, processEpoch, carrierEndpoint.Host, time.Now().UTC(), control.DefaultProcessCarrierServerCertificateLifetime)
	if err != nil {
		return nil, fmt.Errorf("mint process carrier server trust: %w", err)
	}
	distributionPrivateKey, ok := carrierTrust.Certificate.PrivateKey.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("mint process carrier server trust: private key is not Ed25519")
	}
	distributionSigner, err := control.NewEd25519DistributionRequestSigner(control.DistributionProofSignerConfig{
		PrivateKey: distributionPrivateKey, NodeID: cfg.NodeID, ProcessEpoch: processEpoch,
	})
	if err != nil {
		return nil, fmt.Errorf("configure certificate distribution proof: %w", err)
	}
	if err := client.SetDistributionRequestSigner(distributionSigner); err != nil {
		return nil, fmt.Errorf("configure certificate distribution proof: %w", err)
	}
	carrierTrustLifetime, err := newCarrierTrustLifetime(carrierTrust.Certificate, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("schedule process carrier trust refresh: %w", err)
	}
	nodeWorker := &edgeruntime.NodeWorker{Manager: manager, Sink: client, Registration: control.NodeRegistration{NodeID: cfg.NodeID, EdgePool: cfg.EdgePool, Artifact: "paperboat-connector-v1", Protocol: "1.0", ProcessEpoch: processEpoch, Capacity: deployment.NodeCapacity, Endpoint: control.ConnectorEndpoint{Host: deployment.ConnectorAdvertiseHost, TCPPort: carrierEndpoint.TCPPort, QUICPort: carrierEndpoint.QUICPort}, CarrierEndpoint: *carrierEndpoint, CarrierServerSPKISHA256: carrierTrust.SPKISHA256, CarrierServerCertificateChainPEM: carrierTrust.CertificateChainPEM}, Interval: deployment.ControlInterval}
	routeWorker := &edgeruntime.RouteWorker{Registry: canonicalRoutes, LegacyRegistry: routes, Source: client, Observer: client, State: state, NodeID: cfg.NodeID, ProcessEpoch: processEpoch, Carrier: durableRoutes, PublicTCP: publicTCP, DurableAdmissions: durableAdmissions, AccessorSource: client, AccessorAdmissions: accessorAdmissions, Interval: deployment.ControlInterval, DrainTimeout: deployment.ControlTimeout}
	usageWorker := &edgeruntime.UsageWorker{Queue: queue, Sink: client, Prepare: meter, Persist: meter.Persist, Interval: 250 * time.Millisecond}
	metrics := observability.NewMetrics()
	controlDependency := &edgeruntime.ControlDependency{Source: client, TrustSource: client, ApplyTrust: trust.Snapshot.ReplaceRevocations, NodeID: cfg.NodeID, Interval: deployment.ControlInterval}
	trusted, err := edgehttp.ParseTrustedProxies(append(deployment.TrustedProxyCIDRs, "127.0.0.1/32"))
	if err != nil {
		return nil, fmt.Errorf("parse edge trusted proxies: %w", err)
	}
	previewTransport, err := edgehttp.NewDataCarrierPreviewTransport(edgehttp.DataCarrierPreviewTransportConfig{Registry: previewRoutes, StreamOpenTimeout: deployment.ControlTimeout})
	if err != nil {
		return nil, fmt.Errorf("create preview carrier transport: %w", err)
	}
	routeMatcher := edgehttp.NewCompositeRouteMatcher(canonicalRoutes, routes, edgehttp.NewPreviewCarrierRouteMatcher(previewRoutes))
	durableTransport, err := edgehttp.NewDataCarrierRouteTransport(edgehttp.DataCarrierRouteTransportConfig{Registry: durableRoutes, StreamOpenTimeout: deployment.ControlTimeout})
	if err != nil {
		return nil, fmt.Errorf("create durable carrier transport: %w", err)
	}
	carrierTelemetry, err := datacarrier.NewCarrierTelemetry(datacarrier.CarrierTelemetryConfig{
		Metrics: typedMetrics,
		Events:  typedEvents,
		Clock:   time.Now,
		Info: datacarrier.StreamInfo{
			IDs:                edgetelemetry.SafeIDs{EdgeNodeID: cfg.NodeID},
			CorrelationID:      "corr_edge_carrier",
			ReadDirection:      "ingress",
			WriteDirection:     "egress",
			CancellationReason: "shutdown",
		},
		Observe: func(ctx context.Context, operation, outcome, code string, duration time.Duration) {
			if reporter != nil {
				reporter.Observe(ctx, operation, outcome, code, "", duration)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create carrier telemetry: %w", err)
	}
	previewRequestTelemetry, err := edgehttp.NewRequestTelemetry(edgehttp.RequestTelemetryConfig{
		Metrics: typedMetrics,
		Events:  typedEvents,
		Clock:   time.Now,
		Info: edgehttp.RequestInfo{
			IDs:           edgetelemetry.SafeIDs{EdgeNodeID: cfg.NodeID},
			CorrelationID: "corr_edge_request",
			RouteKind:     "preview_public_https_wss",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create preview request telemetry: %w", err)
	}
	durableRequestTelemetry, err := edgehttp.NewRequestTelemetry(edgehttp.RequestTelemetryConfig{
		Metrics: typedMetrics,
		Events:  typedEvents,
		Clock:   time.Now,
		Info: edgehttp.RequestInfo{
			IDs:           edgetelemetry.SafeIDs{EdgeNodeID: cfg.NodeID},
			CorrelationID: "corr_edge_request",
			RouteKind:     "tunnel_https_wss",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create durable request telemetry: %w", err)
	}
	previewForwarder := previewRequestTelemetry.RoundTripper(&carrierTelemetryRoundTripper{
		Next:      previewTransport,
		Telemetry: carrierTelemetry,
		RouteKind: "preview_public_https_wss",
		NodeID:    cfg.NodeID,
	})
	durableForwarder := durableRequestTelemetry.RoundTripper(&carrierTelemetryRoundTripper{
		Next:      durableTransport,
		Telemetry: carrierTelemetry,
		RouteKind: "tunnel_https_wss",
		NodeID:    cfg.NodeID,
	})
	privateConnections, err := edgehttp.NewPrivateAccessConnectionRegistry(int(deployment.NodeCapacity))
	if err != nil {
		return nil, fmt.Errorf("create private connection registry: %w", err)
	}
	var browserAccess *edgehttp.BrowserAccess
	if deployment.BrowserAccessEnabled {
		browserAccess = &edgehttp.BrowserAccess{Authority: &control.BrowserAccessClient{HTTP: client, NodeID: cfg.NodeID, ProcessEpoch: processEpoch}, LoginOrigin: deployment.BrowserLoginOrigin}
	}
	inspectorAccess := &edgehttp.InspectorEdgeAccess{Authority: &control.InspectorAccessClient{HTTP: client, NodeID: cfg.NodeID, ProcessEpoch: processEpoch}, Carriers: durableRoutes, PreviewCarriers: previewRoutes}
	gateway, err := edgehttp.NewGatewayWithTransports(edgehttp.Config{BrowserAccess: browserAccess, InspectorAccess: inspectorAccess, PreviewBaseDomain: deployment.PreviewBaseDomain, TunnelBaseDomain: deployment.TunnelBaseDomain, RuntimeBaseDomain: deployment.RuntimeBaseDomain, TrustedProxies: trusted, MaxHeaderBytes: deployment.MaxHeaderBytes, MaxBodyBytes: deployment.MaxBodyBytes, Routes: routeMatcher, PrivateAccessToken: internalToken, PrivateAccessConnections: privateConnections, Readiness: previewReadiness{Canonical: previewRoutes, Fallback: routes}, HelperAccess: verifier, Revocations: trust.Snapshot, RevocationCheckInterval: deployment.ControlInterval}, "", previewForwarder, durableForwarder)
	if err != nil {
		return nil, fmt.Errorf("create edge gateway: %w", err)
	}
	privateAccessAuthorizer, err := control.NewPrivateAccessGrantClient(client, cfg.NodeID, processEpoch)
	if err != nil {
		return nil, fmt.Errorf("create private access authorizer: %w", err)
	}
	privateAccessBridge, err := edgehttp.NewPrivateAccessStreamBridge(edgehttp.PrivateAccessStreamBridgeConfig{
		Authorizer: privateAccessAuthorizer,
		Target: edgehttp.PrivateAccessRouteTarget{
			HTTP:   edgehttp.PrivateAccessHTTPTarget{Address: deployment.PrivateHTTPSListenAddress, Connections: privateConnections},
			Routes: canonicalRoutes, Carriers: durableRoutes,
		},
		MaximumStreams: int(deployment.NodeCapacity), AuthorizeTimeout: deployment.ControlTimeout, OpenTimeout: deployment.ControlTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("create private access stream bridge: %w", err)
	}
	gatewayRequestTelemetry, err := edgehttp.NewRequestTelemetry(edgehttp.RequestTelemetryConfig{
		Metrics: typedMetrics,
		Events:  typedEvents,
		Clock:   time.Now,
		Info: edgehttp.RequestInfo{
			IDs:           edgetelemetry.SafeIDs{EdgeNodeID: cfg.NodeID},
			CorrelationID: "corr_edge_request",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create gateway request telemetry: %w", err)
	}
	gatewayHandler := gatewayRequestTelemetry.Handler(publicIngressHandler(deployment.PublicRoutes, deployment.ConnectorAdvertiseHost, cfg.HealthAddress, gateway))
	carrierComponent := carrier
	var carrierCleanup func() error
	if carrierComponent == nil {
		carrierComponent, carrierCleanup, err = newCarrierComponentWithTelemetry(deployment, carrierTrust.Certificate, expectedAdmissions, durableAdmissions, accessorAdmissions, previewHandler.Handle, durableRoutes, privateAccessBridge, carrierTelemetry)
		if err != nil {
			return nil, err
		}
	}
	trackedCarrier := &trackedCarrierComponent{Component: carrierComponent}
	carrierComponent = trackedCarrier
	redirectHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host := request.Host
		if name, _, splitErr := net.SplitHostPort(host); splitErr == nil {
			host = name
		}
		http.Redirect(writer, request, "https://"+host+request.URL.RequestURI(), http.StatusPermanentRedirect)
	})
	publicTLS, err := infrastructureTLS(deployment, certificateSelector.TLSConfig())
	if err != nil {
		return nil, err
	}
	gatewayHandler = installationChallenge(cfg.NodeID, deployment.ConnectorAdvertiseHost, credential, gatewayHandler)
	assembly, err := edgeruntime.NewAssembly(edgeruntime.AssemblySpec{Persistence: persistence, Control: controlDependency, Carrier: carrierComponent, Certificates: certificateWorker, Preview: previewWorker, Node: nodeWorker, Routes: routeWorker, PublicTCP: publicTCP, Usage: usageWorker, GatewayAddress: deployment.PublicHTTPSListenAddress, GatewayHandler: gatewayHandler, GatewayTLS: publicTLS, GatewayWrapListener: func(listener net.Listener) (net.Listener, error) {
		return publicTCP.WrapTLSListener(listener, func(host string) bool {
			if host == deployment.ConnectorAdvertiseHost {
				return true
			}
			for _, entry := range deployment.PublicRoutes {
				if host == entry.Host {
					return true
				}
			}
			return routeMatcher.HasHTTPSHostname(host)
		})
	}, GatewayMaxHeaderBytes: int(deployment.MaxHeaderBytes), PrivateAddress: deployment.PrivateHTTPSListenAddress, PrivateHandler: gatewayHandler, PrivateTLS: certificateSelector.TLSConfig(), RedirectAddress: deployment.PublicHTTPListenAddress, RedirectHandler: redirectHandler})
	if err != nil {
		if carrierCleanup != nil {
			_ = carrierCleanup()
		}
		return nil, err
	}
	health, err := observability.NewHandler(observability.Sources{
		Node:          state.Snapshot,
		Manager:       manager.Snapshot,
		Sessions:      func() int { return durableRoutes.Stats().Sessions },
		SessionRoutes: func() int { return durableRoutes.Stats().Routes },
		ActiveStreams: func() uint32 { return durableRoutes.Stats().ActiveStreams },
		RouteCount: func() int {
			_, canonical, _ := canonicalRoutes.CanonicalSnapshot()
			return len(routes.Snapshot()) + len(canonical)
		},
		Usage:          queue.Stats,
		ControlErr:     nodeWorker.LastError,
		RouteErr:       routeWorker.LastError,
		UsageErr:       usageWorker.LastError,
		CarrierRunning: trackedCarrier.running.Load,
		Events:         metrics.Snapshot,
		Traffic:        counters.Snapshot,
		Health:         typedHealth.Snapshot,
		Lifecycle:      typedEvents.Snapshot,
		TypedMetrics:   typedMetrics.Snapshot,
		TelemetryDrops: func() uint64 { return canonicalRoutes.TelemetryDrops() + typedEvents.Dropped() },
	})
	if err != nil {
		if carrierCleanup != nil {
			_ = carrierCleanup()
		}
		return nil, fmt.Errorf("create private observability handler: %w", err)
	}
	service := edgeruntime.New(cfg, state, telemetryLifecycle, assembly, carrierTrustLifetime, telemetryReady{lifecycle: telemetryLifecycle})
	if err := service.SetHealthHandler(health); err != nil {
		if carrierCleanup != nil {
			_ = carrierCleanup()
		}
		return nil, err
	}
	certificateOwned = false
	eventLogOwned = false
	return service, nil
}

func carrierEndpointFromDeployment(deployment config.Deployment) (*control.ConnectorEndpoint, error) {
	if deployment.CarrierTCPListenAddress == "" || deployment.CarrierQUICListenAddress == "" {
		return nil, errors.New("both dedicated carrier listeners are required")
	}
	_, tcpPortText, err := net.SplitHostPort(deployment.CarrierTCPListenAddress)
	if err != nil {
		return nil, errors.New("carrier TCP listener is invalid")
	}
	_, quicPortText, err := net.SplitHostPort(deployment.CarrierQUICListenAddress)
	if err != nil {
		return nil, errors.New("carrier QUIC listener is invalid")
	}
	tcpPort, err := strconv.ParseUint(tcpPortText, 10, 16)
	if err != nil || tcpPort == 0 {
		return nil, errors.New("carrier TCP port is invalid")
	}
	quicPort, err := strconv.ParseUint(quicPortText, 10, 16)
	if err != nil || quicPort == 0 {
		return nil, errors.New("carrier QUIC port is invalid")
	}
	if tcpPort == quicPort {
		return nil, errors.New("carrier TCP and QUIC ports must differ")
	}
	if deployment.ConnectorAdvertiseHost == "" {
		return nil, errors.New("carrier advertised host is required")
	}
	return &control.ConnectorEndpoint{Host: deployment.ConnectorAdvertiseHost, TCPPort: uint16(tcpPort), QUICPort: uint16(quicPort)}, nil
}

type carrierTelemetryRoundTripper struct {
	Next      http.RoundTripper
	Telemetry *datacarrier.CarrierTelemetry
	RouteKind string
	NodeID    string
	sequence  atomic.Uint64
}

func (t *carrierTelemetryRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.Next == nil {
		return nil, edgehttp.ErrInvalidRequestTelemetry
	}
	if request == nil {
		return t.Next.RoundTrip(request)
	}
	if t.Telemetry == nil {
		return t.Next.RoundTrip(request)
	}

	var response *http.Response
	stream, err := t.Telemetry.Open(request.Context(), t.streamInfo(request), func(ctx context.Context) (io.ReadWriteCloser, error) {
		result, roundTripErr := t.Next.RoundTrip(request.WithContext(ctx))
		if roundTripErr != nil {
			if result != nil && result.Body != nil {
				_ = result.Body.Close()
			}
			return nil, roundTripErr
		}
		if result == nil {
			return nil, errors.New("carrier transport returned no response")
		}
		if result.Body == nil {
			result.Body = http.NoBody
		}
		response = result
		return &carrierResponseStream{body: result.Body}, nil
	})
	if err != nil {
		return nil, err
	}
	if response == nil {
		_ = stream.Close()
		return nil, errors.New("carrier telemetry opened without a response")
	}
	response.Body = stream
	return response, nil
}

func (t *carrierTelemetryRoundTripper) streamInfo(request *http.Request) datacarrier.StreamInfo {
	protocol, kind := "https", "https"
	if request != nil && strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") {
		protocol, kind = "websocket", "websocket"
	}
	correlationID := "corr_edge_carrier_" + strconv.FormatUint(t.sequence.Add(1), 10)
	info := datacarrier.StreamInfo{
		IDs:           edgetelemetry.SafeIDs{EdgeNodeID: t.NodeID},
		CorrelationID: correlationID, RouteKind: t.RouteKind,
		Protocol: protocol, Kind: kind, ReadDirection: "ingress", WriteDirection: "egress",
		CancellationReason: "shutdown",
	}
	if request == nil {
		return info
	}
	if requestInfo, ok := edgehttp.RequestTelemetryInfo(request.Context()); ok {
		if requestInfo.IDs != (edgetelemetry.SafeIDs{}) {
			info.IDs = requestInfo.IDs
		}
		if requestInfo.Generations != (edgetelemetry.Generations{}) {
			info.Generations = requestInfo.Generations
		}
		if requestInfo.CorrelationID != "" {
			info.CorrelationID = requestInfo.CorrelationID
		}
		if requestInfo.RouteKind != "" {
			info.RouteKind = requestInfo.RouteKind
		}
		if requestInfo.Protocol != "" {
			info.Protocol = requestInfo.Protocol
		}
	}
	return info
}

type carrierResponseStream struct{ body io.ReadCloser }

func (s *carrierResponseStream) Read(payload []byte) (int, error) {
	if s == nil || s.body == nil {
		return 0, io.EOF
	}
	return s.body.Read(payload)
}

func (s *carrierResponseStream) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func (s *carrierResponseStream) Close() error {
	if s == nil || s.body == nil {
		return nil
	}
	return s.body.Close()
}

type telemetryLifecycle struct {
	health     *edgetelemetry.HealthTracker
	metrics    *edgetelemetry.Metrics
	events     *edgetelemetry.EventLog
	clock      func() time.Time
	reporter   *reporting.Reporter
	usageQueue func() usage.QueueStats

	mu           sync.Mutex
	started      bool
	ready        bool
	closed       bool
	startedAt    time.Time
	metricCancel context.CancelFunc
	metricDone   chan struct{}
}

func newTelemetryLifecycle(health *edgetelemetry.HealthTracker, metrics *edgetelemetry.Metrics, events *edgetelemetry.EventLog) *telemetryLifecycle {
	return &telemetryLifecycle{health: health, metrics: metrics, events: events, clock: time.Now}
}

func (l *telemetryLifecycle) Start(ctx context.Context) error {
	if l == nil {
		return errors.New("telemetry lifecycle is unavailable")
	}
	if ctx == nil {
		return errors.New("telemetry lifecycle context is nil")
	}
	if err := ctx.Err(); err != nil {
		_ = l.closeEventLog(context.Background())
		return err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return errors.New("telemetry lifecycle is closed")
	}
	if l.started {
		l.mu.Unlock()
		return errors.New("telemetry lifecycle already started")
	}
	l.started = true
	l.startedAt = l.now()
	if l.reporter != nil {
		var metricCtx context.Context
		metricCtx, l.metricCancel = context.WithCancel(context.Background())
		l.metricDone = make(chan struct{})
		go l.runMetricExport(metricCtx)
	}
	l.mu.Unlock()
	if err := l.updateHealth(edgetelemetry.StatusDegraded, "service_starting", "Edge service is starting.", "Wait for the edge service to become ready.", edgetelemetry.RetryWaitForChange); err != nil {
		_ = l.Shutdown(context.Background())
		return err
	}
	return l.record("edge_service_starting", edgetelemetry.SeverityInfo, edgetelemetry.OutcomeStateChange, "Edge service startup began.")
}

func (l *telemetryLifecycle) MarkReady(ctx context.Context) error {
	if l == nil {
		return errors.New("telemetry lifecycle is unavailable")
	}
	if ctx == nil {
		return errors.New("telemetry lifecycle context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	if l.closed || !l.started {
		l.mu.Unlock()
		return errors.New("telemetry lifecycle is not running")
	}
	if l.ready {
		l.mu.Unlock()
		return nil
	}
	l.ready = true
	l.mu.Unlock()
	if err := l.updateHealth(edgetelemetry.StatusReady, "service_ready", "Edge service is ready.", "No action is required.", edgetelemetry.RetryNone); err != nil {
		return err
	}
	return l.record("edge_service_ready", edgetelemetry.SeverityInfo, edgetelemetry.OutcomeStateChange, "Edge service became ready.")
}

func (l *telemetryLifecycle) Shutdown(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	started := l.started
	cancel, done := l.metricCancel, l.metricDone
	l.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	l.exportMetrics(context.Background())

	var failures []error
	if started {
		if err := l.updateHealth(edgetelemetry.StatusDown, "service_shutdown", "Edge service is shutting down.", "Start the edge service again when it is needed.", edgetelemetry.RetryNotRetryable); err != nil {
			failures = append(failures, err)
		}
		if err := l.record("edge_service_shutdown", edgetelemetry.SeverityInfo, edgetelemetry.OutcomeStateChange, "Edge service shutdown began."); err != nil {
			failures = append(failures, err)
		}
	}
	if err := l.closeEventLog(ctx); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (l *telemetryLifecycle) runMetricExport(ctx context.Context) {
	defer close(l.metricDone)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.exportMetrics(ctx)
		}
	}
}
func (l *telemetryLifecycle) exportMetrics(ctx context.Context) {
	if l == nil || l.reporter == nil || l.metrics == nil {
		return
	}
	l.reporter.ExportDrops(ctx)
	snapshots := make([]reporting.Snapshot, 0)
	for _, sample := range l.metrics.Snapshot() {
		labels := make(map[string]string, len(sample.Labels))
		for _, label := range sample.Labels {
			labels[label.Name] = label.Value
		}
		switch sample.Kind {
		case edgetelemetry.MetricCounter:
			snapshots = append(snapshots, reporting.Snapshot{Name: sample.Name, Kind: "counter", Value: float64(sample.Value), Labels: labels})
		case edgetelemetry.MetricGauge:
			snapshots = append(snapshots, reporting.Snapshot{Name: sample.Name, Kind: "gauge", Value: float64(sample.Value), Labels: labels})
		case edgetelemetry.MetricHistogram:
			snapshots = append(snapshots, reporting.Snapshot{Name: sample.Name, Kind: "histogram_sum", Value: sample.Sum, Labels: labels}, reporting.Snapshot{Name: sample.Name, Kind: "histogram_count", Value: float64(sample.Count), Labels: labels})
		}
	}
	if l.usageQueue != nil {
		pending := l.usageQueue()
		snapshots = append(snapshots, reporting.Snapshot{Name: edgetelemetry.MetricQueueDepth, Kind: "gauge", Value: float64(pending.Reports), Labels: map[string]string{"queue": "usage"}})
	}
	l.reporter.MetricSnapshots(ctx, snapshots)
}

func (l *telemetryLifecycle) updateHealth(status edgetelemetry.HealthStatus, code, summary, repair string, retry edgetelemetry.RetryDecision) error {
	if l == nil || l.health == nil {
		return nil
	}
	return l.health.Update(edgetelemetry.HealthUpdate{
		Dimension: edgetelemetry.DimensionService, Status: status, Code: code,
		Summary: summary, RepairAction: repair, CorrelationID: "corr_edge_lifecycle", Retry: retry,
	})
}

func (l *telemetryLifecycle) record(name string, severity edgetelemetry.EventSeverity, outcome edgetelemetry.EventOutcome, message string) error {
	if l != nil && l.reporter != nil {
		observed, code := string(outcome), "ok"
		if outcome == edgetelemetry.OutcomeFailed {
			code = "internal"
		}
		l.reporter.Observe(context.Background(), "service_lifecycle", observed, code, "", 0)
	}
	if l == nil || l.events == nil {
		return nil
	}
	_, _, err := l.events.TryRecord(edgetelemetry.EventInput{
		At: l.now(), Severity: severity, Component: edgetelemetry.DimensionService,
		Name: name, Code: name, Outcome: outcome, Message: message,
		CorrelationID: "corr_edge_lifecycle", Retry: edgetelemetry.RetryNone,
	})
	return err
}

func (l *telemetryLifecycle) closeEventLog(ctx context.Context) error {
	if l == nil || l.events == nil {
		return nil
	}
	flushErr := l.events.Flush(ctx)
	closeErr := l.events.Close()
	return errors.Join(flushErr, closeErr)
}

func (l *telemetryLifecycle) now() time.Time {
	at := time.Now().UTC()
	if l != nil && l.clock != nil {
		at = l.clock().UTC()
	}
	if at.IsZero() {
		return time.Now().UTC()
	}
	return at
}

type telemetryReady struct{ lifecycle *telemetryLifecycle }

func (r telemetryReady) Start(ctx context.Context) error {
	if r.lifecycle == nil {
		return errors.New("telemetry lifecycle is unavailable")
	}
	return r.lifecycle.MarkReady(ctx)
}

func (telemetryReady) Shutdown(context.Context) error { return nil }

func controlTLS(caPath string) (*tls.Config, error) {
	if caPath == "" {
		return &tls.Config{MinVersion: tls.VersionTLS13}, nil
	}
	data, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("control CA file contains no certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}, nil
}

func restoreState(path string) (*operation.Journal, *usage.Counters, *usage.Queue, error) {
	saved, err := store.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		journal, journalErr := operation.NewJournal(4096)
		queue, queueErr := usage.NewQueue(4096, 64<<20)
		return journal, usage.NewCounters(), queue, errors.Join(journalErr, queueErr)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	journal, err := operation.Restore(saved.Operations, 4096)
	if err != nil {
		return nil, nil, nil, err
	}
	queue, err := usage.RestoreQueue(saved.PendingUsage, 4096, 64<<20)
	return journal, usage.RestoreCounters(saved.Counters), queue, err
}

func readCredential(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return "", errors.New("invalid control credential file")
	}
	data, err := os.ReadFile(path)
	value := strings.TrimSpace(string(data))
	if err != nil || len(value) < 32 || len(value) > 8192 {
		return "", errors.New("invalid control credential file")
	}
	return value, nil
}
