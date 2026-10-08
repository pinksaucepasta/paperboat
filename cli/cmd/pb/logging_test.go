package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestProcessLoggingResolvesDaemonBehindGlobalFlags(t *testing.T) {
	for _, args := range [][]string{
		{"daemon", "run"},
		{"--config", "/private/config.json", "daemon", "run"},
		{"--json", "--no-customization", "daemon", "status"},
	} {
		if processComponent(args) != "paperboatd" {
			t.Fatal("daemon global flags changed its diagnostic component")
		}
	}
	if processComponent([]string{"--config", "daemon", "status"}) != "pb" {
		t.Fatal("a flag value was mistaken for the daemon command")
	}
}

func TestConfigureProcessLoggingSuppressesInteractiveTransportLogs(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)

	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	recorder := diagnostics.NewMemoryRecorder()
	defer configureProcessLogging([]string{"victus"}, recorder, supportref.New())()
	slog.Info("peer transport detail")
	if output.Len() != 0 {
		t.Fatalf("interactive transport log reached process output: %q", output.String())
	}
	if events := recorder.Recent(); len(events) != 1 || events[0].Fields["source_file"] != "logging_test.go" {
		t.Fatalf("local source evidence missing: %#v", events)
	}
}

func TestConfigureProcessLoggingSuppressesSSHProxyLogs(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)

	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	defer configureProcessLogging([]string{"__ssh-proxy"}, diagnostics.NewMemoryRecorder(), supportref.New())()
	slog.Info("proxy transport detail")
	if output.Len() != 0 {
		t.Fatalf("SSH proxy log reached process output: %q", output.String())
	}
}

type forbiddenDiagnosticValue struct{ called *bool }

func (v forbiddenDiagnosticValue) LogValue() slog.Value {
	*v.called = true
	panic("private diagnostic value was evaluated")
}

func TestProcessLoggerNeverFormatsPrivateMessagesOrAttributes(t *testing.T) {
	recorder := diagnostics.NewMemoryRecorder()
	defer configureProcessLogging([]string{"daemon"}, recorder, supportref.New())()
	called := false
	slog.Default().With("credential", forbiddenDiagnosticValue{called: &called}).WithGroup("private").Error("secret payload example", "error", errors.New("private error contents"))
	events := recorder.Recent()
	if called || len(events) != 1 || events[0].Severity != "error" || events[0].Fields["component"] != "paperboat-daemon" {
		t.Fatalf("unsafe or missing local evidence: called=%v events=%#v", called, events)
	}
	for key, value := range events[0].Fields {
		if strings.Contains(key+value, "private") || strings.Contains(key+value, "secret") || strings.Contains(key+value, "credential") {
			t.Fatalf("private attribute retained in %s", key)
		}
	}
}

func TestDisabledReporterStoresFaultInProcessRecorder(t *testing.T) {
	recorder := diagnostics.NewMemoryRecorder()
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { _ = recorder.RecordFault(fault) })
	defer restore()
	reference := supportref.New()
	ctx := supportref.WithContext(context.Background(), reference)
	var reporter *errorreport.Reporter
	fault := reporter.CaptureFailure(ctx, "pb", "command", "command", "unexpected_cli_failure", context.DeadlineExceeded)
	events := recorder.Recent()
	if len(events) != 1 || events[0].SupportReference != reference || events[0].Fields["cause"] != "deadline_exceeded" || events[0].Code != fault.Code {
		t.Fatalf("disabled reporter discarded local failure: %#v", events)
	}
}
