package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	pbgithub "github.com/pinksaucepasta/paperboat/server/internal/github"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func normalizeHelperIssuer(raw string) string {
	return config.NormalizeIssuer(raw)
}

func mintKeyProvider(cfg config.Config) (*mint.Provider, error) {
	var provider *mint.Provider
	var err error
	if len(cfg.Secrets.MintSigningKeys) > 0 {
		provider, err = mint.ParseKeys(cfg.Secrets.MintSigningKeys, cfg.CLIAuth.MintActiveKeyID, cfg.CLIAuth.MintJWKSMaxAge)
	} else if cfg.Environment != config.EnvironmentProduction {
		provider, err = mint.NewEphemeral(cfg.CLIAuth.MintJWKSMaxAge)
	} else {
		return nil, errors.New("mint signing keys are not configured")
	}
	if err != nil {
		return nil, err
	}
	if err = provider.Withdraw(cfg.CLIAuth.MintWithdrawnKeyIDs); err != nil {
		return nil, err
	}
	if err = provider.ConfigureAdmissionLifetime(cfg.CLIAuth.MintProofLifetime); err != nil {
		return nil, err
	}
	return provider, nil
}

func githubClient(cfg config.Config) pbgithub.Client {
	return pbgithub.HTTPClient{
		BaseURL:  cfg.Providers.GitHub.BaseURL,
		TokenURL: cfg.GitHub.OAuthTokenURL,
		Client:   providerHTTPClient("github", cfg.HTTP.RequestTimeout),
	}
}

func workOSVerifier(cfg config.Config) auth.HTTPWorkOSVerifier {
	return auth.HTTPWorkOSVerifier{
		BaseURL:    cfg.Providers.WorkOS.BaseURL,
		ClientID:   cfg.Secrets.WorkOSClientID,
		APIKey:     cfg.Secrets.WorkOSAPIKey,
		HTTPClient: providerHTTPClient("workos", cfg.HTTP.RequestTimeout),
	}
}

func providerHTTPClient(provider string, timeout time.Duration) *http.Client {
	//paperboat:allow-source-policy default-http owner=server-runtime reason=instrument-standard-transport-behind-configured-timeout
	return &http.Client{Timeout: timeout, Transport: serverdiagnostics.InstrumentProviderTransport(provider, http.DefaultTransport)}
}

// helperHTTPClient carries the server's own requests to machines: runtime hosts are dialed at an edge
// that can serve them now (runtimeattachment.EdgeDialer), so an edge restart does not fail them.
func helperHTTPClient(store *db.DB, timeout time.Duration) *http.Client {
	//paperboat:allow-source-policy default-http owner=server-runtime reason=instrument-standard-transport-behind-configured-timeout
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = runtimeattachment.EdgeDialer{DB: store, Dial: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, Wait: 5 * time.Second}.DialContext
	return &http.Client{Timeout: timeout, Transport: serverdiagnostics.InstrumentProviderTransport("helper", transport)}
}

func publicURLSecure(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https"
}

func (a *App) Run(ctx context.Context) error {
	defer a.healthMonitor.Close()
	if ctx == nil {
		ctx = context.Background()
	}
	stopReporting := func() {}
	if a.supportReports != nil {
		reportingCtx, cancelReporting := context.WithCancel(context.Background())
		reportingDone := make(chan struct{})
		go func() { defer close(reportingDone); _ = a.supportReports.Run(reportingCtx) }()
		stopReporting = func() { cancelReporting(); <-reportingDone }
		defer stopReporting()
	}
	runContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	selfhostContext := selfhostReportingContext(runContext, a.supportReports)
	selfhostDone := make(chan struct{})
	go func() {
		defer close(selfhostDone)
		if a.selfhost != nil {
			a.selfhost.Run(selfhostContext)
		}
	}()
	defer func() { cancelRun(); <-selfhostDone }()
	telemetryEvents, telemetryErr := telemetry.NewEventLog(1024)
	if telemetryErr != nil {
		return fmt.Errorf("start telemetry event log: %w", telemetryErr)
	}
	telemetryEvents.SetObserver(a.supportReports.ObserveEvent)
	a.telemetryEvents = telemetryEvents
	a.telemetryHTTP.Events = telemetryEvents
	if a.telemetryDiagnostics != nil {
		a.telemetryDiagnostics.SetEventLog(telemetryEvents)
	}
	a.telemetryProducer.Events = telemetryEvents
	defer telemetryEvents.Close()
	a.telemetryStartedAt = time.Now().UTC()
	if a.telemetryMetrics != nil {
		_ = a.telemetryMetrics.IncCounter(telemetry.MetricServiceRestarts, telemetry.MetricLabels{"outcome": "success"})
	}
	a.updateServiceTelemetry(telemetry.StatusReady, "running", "Control-plane service is running.", "", telemetry.RetryNone, time.Time{})
	serverErrors := make(chan error, 1)
	workerErrors := make(chan error, 1)
	go func() {
		a.logger.Info("http server starting", "address", a.cfg.HTTP.Address)
		if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
			return
		}
		serverErrors <- nil
	}()
	// A rollout signals the retired server with SIGUSR1 once traffic moved: it stops its workers and
	// releases worker leadership to the new version, while it keeps serving its held connections.
	workerContext, drainWorkers := context.WithCancel(runContext)
	defer drainWorkers()
	drainSignal := make(chan os.Signal, 1)
	signal.Notify(drainSignal, syscall.SIGUSR1)
	defer signal.Stop(drainSignal)
	workersDrained := make(chan struct{})
	go func() {
		select {
		case <-drainSignal:
			a.logger.Info("server draining: stopping background workers")
			drainWorkers()
		case <-workerContext.Done():
		}
	}()
	go func() {
		err := a.worker.Run(workerContext)
		if runContext.Err() == nil && workerContext.Err() != nil {
			setWorkerMode(a.telemetryMetrics, "drained")
			a.logger.Info("server drained: background workers stopped")
			close(workersDrained)
			return
		}
		workerErrors <- err
	}()

	var runErr error
	failureCode := "service_failure"
	var workerErr error
	workerDone := false
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
	case err := <-serverErrors:
		failureCode = "http_server_failed"
		if err != nil {
			runErr = err
			a.updateServiceTelemetry(telemetry.StatusDown, "http_server_failed", "The control-plane HTTP server stopped unexpectedly.", "Restart paperboat-server and inspect correlated telemetry.", telemetry.RetryScheduled, time.Now().UTC().Add(time.Second))
		}
	case err := <-workerErrors:
		failureCode = "worker_failed"
		workerDone = true
		workerErr = err
		if err != nil && supportreport.ProjectFault(ctx, "paperboat-server", "service_lifecycle", "service_error", "service_failed", err).Cause != "context_canceled" {
			runErr = err
			a.updateServiceTelemetry(telemetry.StatusDown, "worker_failed", "A required control-plane worker stopped unexpectedly.", "Restart paperboat-server and inspect correlated telemetry.", telemetry.RetryScheduled, time.Now().UTC().Add(time.Second))
		}
	}

	if runErr != nil && supportreport.ProjectFault(ctx, "paperboat-server", "service_lifecycle", "service_error", "service_failed", runErr).Cause != "context_canceled" && failureCode != "worker_failed" && a.supportReports != nil {
		captureLifecycleFailure(a.supportReports, "service_error", failureCode, runErr)
	}

	// Stop and join workers before closing the certificate hub or database. A
	// worker may still be holding an issuance/distribution reference after the
	// HTTP server has failed, so closing those components first creates a race
	// and can lose a terminal cleanup operation.
	cancelRun()
	workerShutdownCtx, cancelWorkerShutdown := context.WithTimeout(context.Background(), a.cfg.HTTP.ShutdownTimeout)
	if !workerDone {
		select {
		case workerErr = <-workerErrors:
			workerDone = true
		case <-workersDrained:
			workerDone = true
		case <-workerShutdownCtx.Done():
			runErr = errors.Join(runErr, fmt.Errorf("stop workers: %w", workerShutdownCtx.Err()))
		}
	}
	workerJoinErr := a.worker.Wait(workerShutdownCtx)
	if workerJoinErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("join workers: %w", workerJoinErr))
	}
	cancelWorkerShutdown()
	if workerErr != nil && supportreport.ProjectFault(ctx, "paperboat-server", "service_lifecycle", "service_error", "service_failed", workerErr).Cause != "context_canceled" && runErr == nil {
		runErr = workerErr
	}

	serverShutdownCtx, cancelServerShutdown := context.WithTimeout(context.Background(), a.cfg.HTTP.ShutdownTimeout)
	serverShutdownErr := a.server.Shutdown(serverShutdownCtx)
	cancelServerShutdown()
	if workerJoinErr != nil || serverShutdownErr != nil {
		// A child worker or HTTP handler may still hold the certificate hub or
		// database. Returning without closing those shared dependencies is safer
		// than racing live work; process teardown remains the final containment
		// boundary after a bounded graceful shutdown fails.
		if a.supportReports != nil {
			captureLifecycleFailure(a.supportReports, "service_shutdown", "service_failed", errors.Join(workerJoinErr, serverShutdownErr))
		}
		return errors.Join(runErr, serverShutdownErr)
	}
	var runtimeCloseErr error
	if a.certificateRuntime != nil {
		runtimeCloseErr = a.certificateRuntime.Close()
	}
	if runErr == nil || supportreport.ProjectFault(ctx, "paperboat-server", "service_lifecycle", "service_error", "service_failed", runErr).Cause == "context_canceled" {
		a.updateServiceTelemetry(telemetry.StatusNotApplicable, "stopped", "Control-plane service stopped.", "", telemetry.RetryNone, time.Time{})
	}
	stopReportingAfterTelemetryDrain(telemetryEvents, stopReporting)
	databaseCloseErr := a.db.Close()
	if runtimeCloseErr != nil {
		return fmt.Errorf("close certificate runtime: %w", runtimeCloseErr)
	}
	if databaseCloseErr != nil {
		return fmt.Errorf("close database: %w", databaseCloseErr)
	}
	return runErr
}

func selfhostReportingContext(ctx context.Context, reporter *supportreport.Service) context.Context {
	if reporter == nil {
		return ctx
	}
	return reporter.Context(ctx, "")
}

func stopReportingAfterTelemetryDrain(events *telemetry.EventLog, stopReporting func()) {
	if events != nil {
		_ = events.Close()
	}
	if stopReporting != nil {
		stopReporting()
	}
}

func captureLifecycleFailure(reporter *supportreport.Service, stage, code string, err error) supportreport.Fault {
	if reporter == nil || err == nil {
		return supportreport.Fault{}
	}
	ctx := context.Background()
	reference := supportreport.NewReference()
	ctx = serverdiagnostics.WithCorrelationID(ctx, "cor_server_lifecycle")
	ctx = supportreport.WithFailureIdentity(ctx, "", "", reference)
	ctx = reporter.Context(ctx, reference)
	fault := reporter.CaptureFailureWithStack(ctx, "paperboat-server", "service_lifecycle", stage, code, err, sentry.NewStacktrace())
	reporter.LogFault(ctx, slog.LevelError, "control-plane lifecycle failed", fault)
	return fault
}

func (a *App) updateServiceTelemetry(status telemetry.HealthStatus, code, summary, repair string, retry telemetry.RetryDecision, nextRetryAt time.Time) {
	if a == nil {
		return
	}
	if a.telemetryHealth != nil {
		_ = a.telemetryHealth.Update(telemetry.HealthUpdate{Dimension: telemetry.DimensionService, Status: status, Code: code, Summary: summary, RepairAction: repair, CorrelationID: "cor_server_lifecycle", Retry: retry, NextRetryAt: nextRetryAt})
	}
	if a.telemetryMetrics != nil {
		selected := "stopped"
		if status == telemetry.StatusReady {
			selected = "running"
		} else if status == telemetry.StatusDegraded || status == telemetry.StatusDown {
			selected = "degraded"
		}
		uptime := uint64(0)
		if !a.telemetryStartedAt.IsZero() {
			elapsed := time.Since(a.telemetryStartedAt)
			if elapsed > 0 {
				uptime = uint64(elapsed.Seconds())
			}
		}
		for _, state := range []string{"running", "degraded", "stopped"} {
			value := uint64(0)
			if state == selected {
				value = uptime
			}
			_ = a.telemetryMetrics.SetGauge(telemetry.MetricServiceUptime, telemetry.MetricLabels{"state": state}, value)
		}
	}
	if a.telemetryEvents != nil {
		severity, outcome := telemetry.SeverityInfo, telemetry.OutcomeStateChange
		if status == telemetry.StatusDegraded || status == telemetry.StatusDown {
			severity, outcome = telemetry.SeverityError, telemetry.OutcomeFailed
		}
		_, _ = a.telemetryEvents.Record(telemetry.EventInput{At: time.Now().UTC(), Severity: severity, Component: telemetry.DimensionService, Name: "service_lifecycle", Code: code, Outcome: outcome, Message: summary, CorrelationID: "cor_server_lifecycle", Retry: retry, NextRetryAt: nextRetryAt})
	}
}

type readinessChecker struct {
	cfg config.Config
	db  *db.DB
}

func (r readinessChecker) Ready(ctx context.Context) error {
	if err := r.db.Ping(ctx); err != nil {
		return fmt.Errorf("database is not ready: %w", err)
	}

	if r.cfg.Secrets.WorkOSClientID == "" || r.cfg.Secrets.WorkOSAPIKey == "" || r.cfg.Providers.WorkOS.BaseURL == "" {
		return errors.New("WorkOS is not configured")
	}
	return nil
}

// workerLeader elects the one instance that runs background workers (F21-04).
func workerLeader(store *db.DB, metrics *telemetry.Metrics, logger *slog.Logger) *workers.PostgresLeader {
	host, _ := os.Hostname()
	return &workers.PostgresLeader{
		DB: store.Direct(), Instance: fmt.Sprintf("%s/%d", host, os.Getpid()), Version: buildinfo.Version,
		Retry: workers.LeaderRetry, Check: workers.LeaderCheck, Logger: logger,
		Mode: func(mode string) { setWorkerMode(metrics, mode) },
	}
}

func setWorkerMode(metrics *telemetry.Metrics, mode string) {
	if metrics == nil {
		return
	}
	for _, m := range []string{"leader", "standby", "drained"} {
		value := uint64(0)
		if m == mode {
			value = 1
		}
		_ = metrics.SetGauge(telemetry.MetricWorkerMode, telemetry.MetricLabels{"mode": m}, value)
	}
}
