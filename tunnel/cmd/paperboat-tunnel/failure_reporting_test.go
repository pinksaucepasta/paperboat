package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
	edgetelemetry "github.com/pinksaucepasta/paperboat-tunnel/internal/telemetry"
)

func TestConfigurationFailureReportsSafeLocalFaultOffline(t *testing.T) {
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "false")

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousStderr := os.Stderr
	os.Stderr = writeEnd
	defer func() { os.Stderr = previousStderr }()

	reporter, err := reporting.New("paperboat-tunnel")
	if err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(t.TempDir(), "private-deployment.json")
	if code := execute(reporter, []string{"--node-id", "edge-a", "--deployment-config", privatePath}); code != 1 {
		t.Fatalf("execute exit code = %d", code)
	}
	reporter.Close()
	if err := writeEnd.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(readEnd)
	_ = readEnd.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if strings.Contains(text, privatePath) || strings.Contains(text, "open ") || strings.Contains(text, "no such file") {
		t.Fatalf("failure output leaked local path or raw error: %s", text)
	}
	if !strings.Contains(text, "stage=configure") || !strings.Contains(text, "cause=not_found") || !strings.Contains(text, "support reference support_") {
		t.Fatalf("failure output lacks safe recovery metadata: %s", text)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected structured local fault and console diagnostic: %s", text)
	}
	var event struct {
		Schema           string `json:"schema"`
		Name             string `json:"name"`
		Outcome          string `json:"outcome"`
		Cause            string `json:"cause"`
		SupportReference string `json:"support_reference"`
		CorrelationID    string `json:"correlation_id"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("decode local event %q: %v", lines[0], err)
	}
	if event.Schema != "paperboat.edge_event.v1" || event.Name != "tunnel_service_setup_failed" || event.Outcome != "failed" || event.Cause != "not_found" || !strings.HasPrefix(event.SupportReference, "support_") || event.CorrelationID != event.SupportReference {
		t.Fatalf("unexpected local event: %+v", event)
	}
}

func TestLifecycleLocalAndMemoryShareProcessReferenceOffline(t *testing.T) {
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "false")
	reporter, err := reporting.New("paperboat-tunnel")
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = previous }()
	health, _ := edgetelemetry.NewHealthTracker(time.Now)
	events, _ := edgetelemetry.NewEventLogWithQueue(16, 16)
	lifecycle := newTelemetryLifecycle(health, edgetelemetry.NewMetrics(), events)
	lifecycle.reporter = reporter
	ctx := reporting.WithSupportReference(context.Background(), "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83")
	if err := lifecycle.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	reporter.Close()
	logReportingShutdown(ctx, reporter)
	writer.Close()
	output, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 4 {
		t.Fatalf("lifecycle event count=%d", len(lines))
	}
	for index, line := range lines {
		var event struct{ Code, SupportReference, CorrelationID string }
		var fields map[string]any
		if json.Unmarshal([]byte(line), &fields) != nil {
			t.Fatal("invalid local event")
		}
		event.Code, _ = fields["code"].(string)
		event.SupportReference, _ = fields["support_reference"].(string)
		event.CorrelationID, _ = fields["correlation_id"].(string)
		if event.SupportReference != reporting.SupportReference(ctx) || event.CorrelationID != event.SupportReference || event.Code != []string{"ok", "ready", "shutdown", "telemetry_shutdown"}[index] {
			t.Fatal("lifecycle process correlation/code lost")
		}
	}
	for _, event := range events.Snapshot() {
		if event.CorrelationID != reporting.SupportReference(ctx) {
			t.Fatal("in-memory event reference differs")
		}
	}
	err = serviceFailure{error: &os.PathError{Op: "open", Path: "PRIVATE_PATH", Err: os.ErrNotExist}, definition: "service_build"}
	var path *os.PathError
	if !errors.Is(err, os.ErrNotExist) || !errors.As(err, &path) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("safe service wrapper lost original cause")
	}
}

func TestRouteLifecycleExportsSafeProcessReferenceOffline(t *testing.T) {
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "false")
	reporter, err := reporting.New("paperboat-tunnel")
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = previous }()
	health, _ := edgetelemetry.NewHealthTracker(time.Now)
	events, _ := edgetelemetry.NewEventLogWithQueue(32, 32)
	defer events.Close()
	lifecycle := newTelemetryLifecycle(health, edgetelemetry.NewMetrics(), events)
	lifecycle.reporter = reporter
	lifecycle.processCtx = reporting.WithSupportReference(t.Context(), "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83")
	core, _ := route.NewCoreTelemetrySink(health, lifecycle.metrics, events)
	sink := lifecycle.routeSink(core)
	for _, kind := range []route.LifecycleType{route.LifecycleStageRejected, route.LifecycleActivated} {
		if err := sink.RecordRouteTelemetry(route.RouteTelemetryRecord{At: time.Now(), Type: kind, Generation: 1, CorrelationID: "correlation_resource", IDs: edgetelemetry.SafeIDs{RouteID: "PRIVATE_ROUTE"}}); err != nil {
			t.Fatal(err)
		}
	}
	writer.Close()
	output, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "PRIVATE") || strings.Contains(string(output), "correlation_resource") {
		t.Fatal("resource identity leaked into export")
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 {
		t.Fatalf("local lifecycle count=%d", len(lines))
	}
	for index, line := range lines {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		if fields["support_reference"] != reporting.SupportReference(lifecycle.processCtx) || fields["correlation_id"] != fields["support_reference"] {
			t.Fatal("process correlation lost")
		}
		if fields["code"] != []string{"stage_rejected", "activated"}[index] || fields["retry"] != []string{"wait_for_change", "none"}[index] {
			t.Fatal("finite lifecycle or retry lost")
		}
	}
	if err := events.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, event := range events.Snapshot() {
		if event.CorrelationID != "correlation_resource" || event.IDs.RouteID != "PRIVATE_ROUTE" {
			t.Fatal("internal operational identity changed")
		}
	}
}
