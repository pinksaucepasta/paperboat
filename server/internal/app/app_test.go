package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/puddle/v2"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	pbgithub "github.com/pinksaucepasta/paperboat/server/internal/github"
	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
)

func TestWorkOSVerifierUsesConfiguredAPIKeyForCodeExchange(t *testing.T) {
	cfg := config.Default()
	cfg.Secrets.WorkOSClientID = "client_test"
	cfg.Secrets.WorkOSAPIKey = "sk_test_configured"

	verifier, ok := workOSVerifier(cfg).(auth.HTTPWorkOSVerifier)
	if !ok {
		t.Fatal("real WorkOS verifier was not configured")
	}
	if verifier.ClientID != cfg.Secrets.WorkOSClientID || verifier.APIKey != cfg.Secrets.WorkOSAPIKey {
		t.Fatal("WorkOS code exchange is not configured with the client ID and API key")
	}
}

func TestNewRejectsInjectedCertificateRuntimeWhenManagedCertificatesAreDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Certificates.Enabled = false

	application, err := New(Options{Config: cfg, CertificateRuntime: &tunnelv1.CertificateRuntime{}})
	if application != nil {
		t.Fatal("application created with a certificate runtime while managed certificates are disabled")
	}
	if !errors.Is(err, tunnelv1.ErrCertificateRuntimeUnavailable) {
		t.Fatalf("error = %v, want %v", err, tunnelv1.ErrCertificateRuntimeUnavailable)
	}
}

func TestNewAllowsInjectedCertificateRuntimeWhenManagedCertificatesAreEnabled(t *testing.T) {
	cfg := config.Default()
	configureAppTestDatabase(t, &cfg)
	cfg.Certificates.Enabled = true
	runtime := &tunnelv1.CertificateRuntime{}

	application, err := New(Options{Config: cfg, CertificateRuntime: runtime})
	if err != nil {
		t.Fatalf("New rejected an injected runtime with managed certificates enabled: %v", err)
	}
	if application == nil || application.certificateRuntime != runtime {
		t.Fatal("injected certificate runtime was not transferred to the application")
	}
	if err := application.db.Close(); err != nil {
		t.Fatalf("close application database: %v", err)
	}
}

func TestCertificateRuntimePreservesSecretResolutionCancellation(t *testing.T) {
	cfg := config.Default()
	cfg.Certificates = config.Certificates{
		Enabled:                         true,
		DirectoryURL:                    "https://acme.example/directory",
		Issuer:                          "test-issuer",
		AccountKeyReference:             "secret://test/account",
		MasterKeyReference:              "secret://test/master",
		DNSProvider:                     "cloudflare",
		DNSZoneID:                       "zone-test",
		PublicDNSZoneID:                 "zone-test",
		DNSTokenReference:               "secret://test/dns",
		ChallengeZone:                   "challenge.example",
		CAAResolver:                     "127.0.0.1:53",
		DistributionCredentialReference: "secret://test/distribution",
		OwnerID:                         "owner-test",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtime, err := newCertificateRuntime(ctx, &db.DB{}, cfg)
	if runtime != nil {
		t.Fatal("runtime created without a distribution credential")
	}
	if !errors.Is(err, tunnelv1.ErrCertificateRuntimeUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("certificate setup lost secret-resolution cancellation: %v", err)
	}
}

func TestRunShutsDownHTTPServerBeforeReturningWorkerError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	workerErr := errors.Join(context.Canceled, syscall.EIO)
	cfg := config.Default()
	configureAppTestDatabase(t, &cfg)
	cfg.HTTP.Address = address
	cfg.HTTP.ShutdownTimeout = time.Second
	app, err := New(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	app.worker = workers.NewSupervisor(func(context.Context) error {
		return workerErr
	})

	err = app.Run(context.Background())
	if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("error = %v, want canceled worker exit plus EIO", err)
	}
	serviceHealth := app.telemetryHealth.Snapshot().Dimensions.Service
	if serviceHealth.Status != telemetry.StatusDown || serviceHealth.Code != "worker_failed" || serviceHealth.CorrelationID != "cor_server_lifecycle" {
		t.Fatalf("service health=%+v", serviceHealth)
	}
	if events := app.telemetryEvents.Snapshot(); len(events) < 2 || events[len(events)-1].Code != "worker_failed" {
		t.Fatalf("lifecycle events=%+v", events)
	}

	client := http.Client{Timeout: 50 * time.Millisecond}
	resp, err := client.Get("http://" + address + "/healthz")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("server still accepted requests after Run returned")
	}
}

func TestRunPreservesOperationalWorkerFailureJoinedWithCancellation(t *testing.T) {
	workerErr := errors.Join(context.Canceled, syscall.EIO)
	application := &App{
		cfg:               config.Config{HTTP: config.HTTPConfig{ShutdownTimeout: time.Second}},
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		server:            &http.Server{Addr: "127.0.0.1:0"},
		worker:            workers.NewSupervisor(func(context.Context) error { return workerErr }),
		telemetryHTTP:     &telemetry.HTTPObserver{},
		telemetryProducer: &telemetry.Producer{},
	}

	err := application.Run(context.Background())
	if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("Run error = %v, want canceled worker exit plus EIO", err)
	}
}

func TestRunDoesNotCloseWorkerDependenciesBeforeJoin(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	configureAppTestDatabase(t, &cfg)
	cfg.HTTP.Address = address
	cfg.HTTP.ShutdownTimeout = 20 * time.Millisecond
	application, err := New(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	application.worker = workers.NewSupervisor(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		<-release
		return nil
	})

	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- application.Run(runCtx) }()
	<-started
	cancelRun()
	err = <-runDone
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error = %v, want worker join deadline", err)
	}
	acquireCtx, cancelAcquire := context.WithTimeout(context.Background(), 10*time.Millisecond)
	connection, acquireErr := application.db.Pool().Acquire(acquireCtx)
	cancelAcquire()
	if connection != nil {
		connection.Release()
	}
	if errors.Is(acquireErr, puddle.ErrClosedPool) {
		t.Fatal("database closed before worker joined")
	}

	close(release)
	if err := application.worker.Wait(context.Background()); err != nil {
		t.Fatalf("join released worker: %v", err)
	}
	if application.certificateRuntime != nil {
		_ = application.certificateRuntime.Close()
	}
	if err := application.db.Close(); err != nil {
		t.Fatalf("close retained database: %v", err)
	}
}

func TestRunDoesNotCloseHandlerDependenciesBeforeHTTPDrain(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	configureAppTestDatabase(t, &cfg)
	cfg.HTTP.Address = address
	cfg.HTTP.ShutdownTimeout = 20 * time.Millisecond
	application, err := New(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	application.worker = workers.NewSupervisor(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	application.server.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(handlerStarted)
		<-releaseHandler
	})

	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- application.Run(runCtx) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		deadline := time.Now().Add(time.Second)
		for {
			response, requestErr := http.Get("http://" + address)
			if response != nil {
				_ = response.Body.Close()
			}
			if requestErr == nil || time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("HTTP handler did not start")
	}
	cancelRun()
	err = <-runDone
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error = %v, want HTTP drain deadline", err)
	}
	acquireCtx, cancelAcquire := context.WithTimeout(context.Background(), 10*time.Millisecond)
	connection, acquireErr := application.db.Pool().Acquire(acquireCtx)
	cancelAcquire()
	if connection != nil {
		connection.Release()
	}
	if errors.Is(acquireErr, puddle.ErrClosedPool) {
		t.Fatal("database closed before HTTP handler drained")
	}

	close(releaseHandler)
	<-requestDone
	_ = application.server.Close()
	if application.certificateRuntime != nil {
		_ = application.certificateRuntime.Close()
	}
	if err := application.db.Close(); err != nil {
		t.Fatalf("close retained database: %v", err)
	}
}

func TestNormalizeHelperIssuer(t *testing.T) {
	tests := map[string]string{
		"  https://paperboat.example///  ":              "https://paperboat.example",
		"HTTPS://PAPERBOAT.EXAMPLE:443/path/?ignored=1": "https://paperboat.example/path",
		"http://PAPERBOAT.EXAMPLE:80/":                  "http://paperboat.example",
	}
	for input, want := range tests {
		if got := normalizeHelperIssuer(input); got != want {
			t.Errorf("normalizeHelperIssuer(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGitHubClientRequiresCredentials(t *testing.T) {
	cfg := config.Default()
	if _, ok := githubClient(cfg).(pbgithub.HTTPClient); !ok {
		t.Fatal("default development config did not use the HTTP GitHub client")
	}
	service := pbgithub.NewService(nil, nil, githubClient(cfg), cfg)
	if _, err := service.OAuthAuthorizeURL("state", "https://dashboard.example/callback"); !errors.Is(err, pbgithub.ErrClientNotConfigured) {
		t.Fatalf("unconfigured GitHub OAuth = %v", err)
	}
}

// App construction owns the shared quota policy and therefore requires its real
// database boundary. Lifecycle tests run on the approved isolated PostgreSQL DB.
func configureAppTestDatabase(t *testing.T, cfg *config.Config) {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL for application initialization")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	cfg.Database = config.Database{Driver: "postgres", DSN: dsn}
	store, err := db.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
}
