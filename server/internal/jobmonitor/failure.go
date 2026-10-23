package jobmonitor

import (
	"context"
	"log/slog"

	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

const backupFaultMarker = "paperboat.backup_fault.v1"

// ObserveFailure writes the bounded diagnostic projection for one final
// backup-command failure. The underlying error message is never formatted.
func (m *Monitor) ObserveFailure(code string, err error) supportreport.Fault {
	fault, _ := m.projectFailure(backupFaultStage(code), err)
	if err == nil {
		return fault
	}
	m.logFailure(code, fault)
	return fault
}

func (m *Monitor) projectFailure(stage string, err error) (supportreport.Fault, *sentry.Stacktrace) {
	if err == nil {
		return supportreport.Fault{}, nil
	}
	ctx := supportreport.WithFailureIdentity(context.Background(), "", "", supportreport.NewReference())
	stack := sentry.NewStacktrace()
	fault := supportreport.ProjectFaultWithStack(ctx, "paperboat-server", "backup_operation", stage, "service_failed", err, stack)
	return fault, stack
}

func (m *Monitor) logFailure(code string, fault supportreport.Fault) {
	logger := slog.Default()
	if m != nil && m.logger != nil {
		logger = m.logger
	}
	logger.Log(context.Background(), faultLevel(fault), "backup operation failed", append(fault.LogAttrs(), "backup_failure_code", safeBackupFailureCode(code))...)
}

// CaptureFailure is the command's final error owner. It always emits local
// evidence, even when Sentry is disabled, and submits only an event built from
// the same projected fault. Check-in completion and flushing remain owned by
// Start and Close.
func (m *Monitor) CaptureFailure(code string, err error) supportreport.Fault {
	return m.CaptureFailureAt("", code, err)
}

// CaptureFailureAt lets maintenance subcommands identify their finite owning
// phase while sharing the same local/Sentry projection and nil-safe behavior.
func (m *Monitor) CaptureFailureAt(stage, code string, err error) supportreport.Fault {
	stage = safeBackupFaultStage(stage, code)
	fault, stack := m.projectFailure(stage, err)
	if err != nil {
		m.logFailure(code, fault)
	}
	if m == nil || m.client == nil || err == nil || fault.Severity != "error" || fault.Outcome != "failed" {
		return fault
	}
	event := supportreport.NewFaultEvent(fault, stack)
	if event == nil {
		return fault
	}
	event.Tags[backupFaultMarker] = safeBackupFailureCode(code)
	if m.client.CaptureEvent(event, nil, nil) == nil {
		m.submissionsDropped.Add(1)
		m.logDeliveryFailure(safeJobMonitorFailure("backup failure event was not admitted"))
	}
	return fault
}

func safeBackupFaultStage(stage, code string) string {
	switch stage {
	case "backup_execution", "backup_verification", "backup_restore", "backup_cleanup", "backup_reporting":
		return stage
	default:
		return backupFaultStage(code)
	}
}

func backupFaultStage(code string) string {
	switch safeBackupFailureCode(code) {
	case "backup_failed", "storage_unavailable", "unsafe_storage", "storage_limit", "entropy_unavailable", "archive_limit":
		return "backup_execution"
	case "archive_invalid", "backup_unavailable", "invalid_manifest":
		return "backup_verification"
	case "restore_failed":
		return "backup_restore"
	case "verification_cleanup_failed":
		return "backup_cleanup"
	case "report_failed":
		return "backup_reporting"
	default:
		return "service_error"
	}
}

func cleanBackupEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event == nil || safeBackupFailureCode(event.Tags[backupFaultMarker]) != event.Tags[backupFaultMarker] {
		return nil
	}
	return supportreport.SanitizeFaultEvent(event)
}

func safeBackupFailureCode(code string) string {
	switch code {
	case "invalid_configuration", "storage_unavailable", "unsafe_storage", "operation_cancelled", "storage_limit", "entropy_unavailable", "invalid_manifest", "archive_limit", "backup_failed", "archive_invalid", "backup_unavailable", "restore_failed", "verification_cleanup_failed", "report_failed":
		return code
	default:
		return "unknown"
	}
}

type safeJobMonitorFailure string

func (e safeJobMonitorFailure) Error() string { return string(e) }
