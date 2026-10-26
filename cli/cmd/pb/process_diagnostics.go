package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/spf13/cobra"
)

func openProcessDiagnostics(ctx context.Context, args []string, reporter *errorreport.Reporter, stderr io.Writer) (context.Context, func()) {
	var storageErr error
	var recorder *diagnostics.Recorder
	command, _, findErr := newRootCommand().Find(args)
	removesStorage := findErr == nil && command != nil && (command.Name() == "uninstall" || command.Name() == "reset" || runtime.GOOS == "windows" && command.Name() == "__complete-uninstall")
	if !removesStorage && command != nil && command.Name() == "__runtime-service" {
		for i, arg := range args {
			if arg == "__runtime-service" && i+1 < len(args) && args[i+1] == "purge" {
				removesStorage = true
				break
			}
		}
	}
	if removesStorage {
		// These commands remove the diagnostics directory itself. A disk segment
		// would lock that directory on Windows or fail to save at exit. The
		// Windows helper's protected completion status retains failure metadata.
		recorder = diagnostics.NewMemoryRecorder()
	} else {
		var directory string
		directory, storageErr = processDiagnosticsDirectory(command)
		if storageErr == nil {
			recorder, storageErr = diagnostics.NewRecorder(diagnostics.DiskConfig{Directory: directory, OwnerUID: os.Geteuid()})
		}
	}
	if storageErr != nil {
		recorder = diagnostics.NewMemoryRecorder()
	}
	ctx = diagnostics.WithRecorder(ctx, recorder)
	restoreLogger := configureProcessLogging(args, recorder, supportref.FromContext(ctx))
	restoreFaultObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		_ = recorder.RecordFault(fault)
	})
	registerDiagnosticMetrics(reporter, recorder)
	if storageErr != nil {
		reporter.ObserveFailure(ctx, processComponent(args), "process", "diagnostic_storage", "diagnostic_storage_unavailable", storageErr)
		fmt.Fprintf(stderr, "pb: Local diagnostics could not be saved. Run `pb doctor` to check storage permissions. Support reference: %s.\n", supportref.FromContext(ctx))
	}
	return ctx, func() {
		// Command-owned producers have stopped before this process cleanup. One
		// deadline bounds asynchronous logs, disk persistence and SDK flushing.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		defer restoreLogger()
		defer restoreFaultObserver()
		// Leave a short part of the shared budget for persisting the SDK result.
		// A drained SDK queue is evidence of finished attempts, not receipt.
		deadline, _ := shutdownCtx.Deadline()
		exportCtx, stopExport := context.WithDeadline(shutdownCtx, deadline.Add(-250*time.Millisecond))
		queueErr := diagnosticlog.Flush(exportCtx)
		reporter.Flush(exportCtx)
		stopExport()
		severity := "info"
		if reporter.FlushStatus() == "timed_out" || reporter.SDKSubmissionsDropped() != 0 || reporter.SDKHTTPFailures() != 0 {
			severity = "warning"
		}
		_ = recorder.RecordWithSupportReference("shutdown", "telemetry_shutdown", severity, supportref.FromContext(ctx), map[string]string{
			"component": errorreport.Component(processComponent(args)),
			"operation": "component_shutdown", "state": reporter.FlushStatus(),
			"sdk_submissions_dropped": strconv.FormatUint(reporter.SDKSubmissionsDropped(), 10),
			"sdk_http_failures":       strconv.FormatUint(reporter.SDKHTTPFailures(), 10),
		})
		storageErr := recorder.Flush(shutdownCtx)
		closeErr := recorder.CloseContext(shutdownCtx)
		if queueErr != nil || storageErr != nil || closeErr != nil {
			fmt.Fprintf(stderr, "pb: Some local diagnostics could not be saved before shutdown. Run `pb doctor` to check diagnostic storage. Support reference: %s.\n", supportref.FromContext(ctx))
		}
	}
}

func registerDiagnosticMetrics(reporter *errorreport.Reporter, recorder *diagnostics.Recorder) {
	names := []string{
		errorreport.DiagnosticRecordsDroppedMetric,
		errorreport.DiagnosticRecordsFailedMetric,
		errorreport.DiagnosticQueueDroppedMetric,
		errorreport.DiagnosticPersistenceAvailableMetric,
	}
	descriptors := make([]errorreport.MetricDescriptor, len(names))
	for index, name := range names {
		descriptors[index] = errorreport.MetricDescriptor{Name: name}
	}
	reporter.RegisterMetrics(func() []errorreport.MetricSample {
		stats := recorder.Stats()
		available := float64(0)
		if stats.PersistenceAvailable {
			available = 1
		}
		return []errorreport.MetricSample{
			{Name: names[0], Value: float64(stats.DroppedRecords)},
			{Name: names[1], Value: float64(stats.FailedRecords)},
			{Name: names[2], Value: float64(diagnosticlog.Dropped())},
			{Name: names[3], Value: available},
		}
	}, descriptors)
}

// configureProcessLogging preserves interactive and machine output while
// retaining bounded source evidence for library logs. Authored semantic faults
// use CaptureFailure/ObserveFailure to retain their specific code and cause.
func configureProcessLogging(args []string, recorder *diagnostics.Recorder, reference string) func() {
	previous := slog.Default()
	slog.SetDefault(slog.New(processDiagnosticHandler{recorder: recorder, component: errorreport.Component(processComponent(args)), reference: reference}))
	return func() { slog.SetDefault(previous) }
}

type processDiagnosticHandler struct {
	recorder  *diagnostics.Recorder
	component string
	reference string
}

func (h processDiagnosticHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h processDiagnosticHandler) Handle(ctx context.Context, record slog.Record) error {
	severity := "info"
	if record.Level >= slog.LevelError {
		severity = "error"
	} else if record.Level >= slog.LevelWarn {
		severity = "warning"
	}
	fields := map[string]string{"component": h.component, "operation": "log"}
	if record.PC != 0 {
		frame, _ := runtime.CallersFrames([]uintptr{record.PC}).Next()
		if file := sourceIdentifier(filepath.Base(frame.File), 128); file != "" {
			fields["source_file"] = file
		}
		if function := sourceIdentifier(frame.Function, 128); function != "" {
			fields["source_function"] = function
		}
		if frame.Line > 0 {
			fields["source_line"] = strconv.Itoa(frame.Line)
		}
	}
	reference := supportref.FromContext(ctx)
	if reference == "" {
		reference = h.reference
	}
	// Record.Message and Attrs may contain upstream URLs, credentials, payloads,
	// or arbitrary LogValuers. Never resolve, format, retain or export them.
	return h.recorder.RecordWithSupportReference("logging", "unclassified_log", severity, reference, fields)
}

func (h processDiagnosticHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h processDiagnosticHandler) WithGroup(string) slog.Handler      { return h }

func sourceIdentifier(value string, maximum int) string {
	if index := strings.LastIndexByte(value, '/'); index >= 0 {
		value = value[index+1:]
	}
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-", r) {
			return r
		}
		return -1
	}, value)
	if len(value) > maximum {
		value = value[:maximum]
	}
	return value
}

// processDiagnosticsDirectory is the user's state directory, except for the
// root update service and its activation helper: they run under the service
// manager with no HOME and own their own state root, so their diagnostics live
// there instead of failing to save at every update step.
func processDiagnosticsDirectory(command *cobra.Command) (string, error) {
	if command != nil && command.Name() == "__runtime-updated" {
		if root := os.Getenv("PAPERBOAT_UPDATE_STATE_ROOT"); filepath.IsAbs(root) && filepath.Clean(root) == root {
			return filepath.Join(root, "diagnostics"), nil
		}
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return "", err
	}
	return filepath.Join(paths.StateRoot, "diagnostics"), nil
}
