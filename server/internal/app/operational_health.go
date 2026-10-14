package app

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat-server/internal/backupreport"
	"github.com/pinksaucepasta/paperboat-server/internal/jobmonitor"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"
)

func operationalHealthWorker(backups *backupreport.Service, reporter *supportreport.Service, monitor *jobmonitor.Monitor, progress ...*workers.Progress) workers.Worker {
	return operationalHealthWorkerWithCheck(time.Minute, backups.Health, reporter, monitor, progress...)
}

func operationalHealthWorkerWithCheck(interval time.Duration, readHealth func(context.Context) (backupreport.HealthSnapshot, error), reporter *supportreport.Service, monitor *jobmonitor.Monitor, progress ...*workers.Progress) workers.Worker {
	var tracker *workers.Progress
	if len(progress) > 0 {
		tracker = progress[0]
	}
	return func(ctx context.Context) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		observation := backupHealthObservation{reference: supportreport.NewReference()}
		for {
			if ctx.Err() != nil {
				return nil
			}
			samples := tracker.Snapshot()
			workersHealthy, warming := publishWorkerHealth(reporter, samples)
			// Startup is unknown, never a fabricated successful cycle. The monitor's
			// deployment-owned check-in margin must cover the configured startup budget.
			finish := func(bool) {}
			if !warming {
				finish = monitor.Start()
			}
			health, healthErr := readHealth(ctx)
			publishBackupHealth(reporter, health)
			observeBackupHealthResult(reporter, ctx, &observation, healthErr)
			finish(healthErr == nil && !warming && workersHealthy && ctx.Err() == nil && health.Backup.State == backupreport.HealthCurrent && health.Restore.State == backupreport.HealthCurrent)
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}
}

type backupHealthObservation struct {
	reference string
	class     string
	active    bool
}

func observeBackupHealthResult(reporter *supportreport.Service, ctx context.Context, observation *backupHealthObservation, err error) {
	if reporter == nil || observation == nil {
		return
	}
	faultCtx := reporter.Context(ctx, observation.reference)
	const component, operation, stage, code = "paperboat-server", "backup_operation", "service_error", "service_failed"
	if err != nil {
		projected := supportreport.ProjectFault(faultCtx, component, operation, stage, code, err)
		if projected.Cause == "context_canceled" {
			return
		}
		class := strings.Join([]string{projected.Cause, projected.ErrorType, strings.Join(projected.ErrorChain, ","), strconv.Itoa(projected.Errno)}, "\x00")
		if observation.active && class == observation.class {
			return
		}
		fault := reporter.ObserveFailureWithStack(faultCtx, component, operation, stage, code, err, sentry.NewStacktrace())
		reporter.LogFault(faultCtx, slog.LevelWarn, "backup health check failed", fault)
		observation.class, observation.active = class, true
		return
	}
	if observation.active {
		fault := reporter.CaptureFailure(faultCtx, component, operation, stage, "worker_recovered", nil)
		reporter.LogFault(faultCtx, slog.LevelInfo, "backup health check recovered", fault)
		observation.class, observation.active = "", false
	}
}

func publishBackupHealth(reporter *supportreport.Service, health backupreport.HealthSnapshot) {
	reporter.Gauge("paperboat.backup.state", float64(health.Backup.State))
	reporter.Gauge("paperboat.restore.state", float64(health.Restore.State))
	if health.Backup.AgeSeconds != nil {
		reporter.Gauge("paperboat.backup.age_seconds", float64(*health.Backup.AgeSeconds))
	}
	if health.Restore.AgeSeconds != nil {
		reporter.Gauge("paperboat.restore.age_seconds", float64(*health.Restore.AgeSeconds))
	}
}

func publishWorkerHealth(reporter *supportreport.Service, samples []workers.ProgressSample) (healthy, warming bool) {
	healthy = true
	failed := false
	for _, sample := range samples {
		reporter.WorkerProgress(sample.Name, sample.State, sample.AgeSeconds)
		if sample.Warming {
			warming = true
		}
		if !sample.Healthy {
			healthy = false
			if !sample.Warming {
				failed = true
			}
		}
	}
	// A known failure must not be hidden by an unrelated warming worker.
	return healthy, warming && !failed
}
