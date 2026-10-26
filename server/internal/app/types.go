package app

import (
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/jobmonitor"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat/server/internal/teaminbox"
	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
	"log/slog"
	"net/http"
	"time"
)

type Options struct {
	Config config.Config
	Logger *slog.Logger
	// CertificateRuntime is a fully composed managed-certificate lifecycle.
	// App owns it after New succeeds and closes it before the database on
	// shutdown. Supplying this is mutually exclusive with the legacy worker
	// and handler fields below.
	CertificateRuntime *tunnelv1.CertificateRuntime
	// CertificateWorker is supplied only by deployment composition that has
	// configured a real issuer, CAA policy, envelope-key resolver, and
	// authenticated edge distributor.  Nil keeps startup fail-closed without
	// inventing certificate material or provider credentials.
	CertificateWorker *tunnelv1.CertificateWorker
	// CertificateDistribution is the internal authenticated server-to-edge
	// transport used by the certificate worker's distributor. It is mounted
	// only on the edge-control handler and never exposed by the public API.
	CertificateDistribution http.Handler
	// ReceiptEmailSender is the existing deployment-owned email delivery
	// integration. Nil keeps receipt notifications queued and visible without
	// sending user email.
	ReceiptEmailSender teaminbox.ReceiptSender
}

type App struct {
	selfhost             *selfhost.Service
	supportReports       *supportreport.Service
	healthMonitor        *jobmonitor.Monitor
	cfg                  config.Config
	logger               *slog.Logger
	db                   *db.DB
	server               *http.Server
	worker               *workers.Supervisor
	certificateRuntime   *tunnelv1.CertificateRuntime
	telemetryEvents      *telemetry.EventLog
	telemetryMetrics     *telemetry.Metrics
	telemetryHealth      *telemetry.HealthTracker
	telemetryHTTP        *telemetry.HTTPObserver
	telemetryDiagnostics *telemetry.Diagnostics
	telemetryProducer    *telemetry.Producer
	telemetryStartedAt   time.Time
}
