package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
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

func TestProcessLoggerRetainsOnlyAuthoredReconnectMetadata(t *testing.T) {
	recorder := diagnostics.NewMemoryRecorder()
	handler := processDiagnosticHandler{recorder: recorder, component: "pb"}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "peer QUIC close", reflect.ValueOf((*peerquic.Session).Close).Pointer())
	called := false
	record.Add("stage", "application_close_start", "trace_id", "test_trace", "credential", "PRIVATE", "failed", forbiddenDiagnosticValue{called: &called})
	if err := handler.Handle(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	events := recorder.Recent()
	if called || len(events) != 1 || events[0].Code != "reconnect_transport" || events[0].Fields["stage"] != "application_close_start" || events[0].Fields["observed_unix_us"] == "" {
		t.Fatal("recovery metadata was discarded or private value evaluated")
	}
	for key, value := range events[0].Fields {
		if strings.Contains(key+value, "PRIVATE") || key == "credential" {
			t.Fatal("private attribute retained")
		}
	}
	record = slog.NewRecord(time.Now(), slog.LevelInfo, "native underlay packet", reflect.ValueOf((*mesh.Server).Start).Pointer())
	record.Add("packet_type", 1, "direction", "egress", "engine_label", "opaque", "credential", "PRIVATE")
	if err := handler.Handle(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	events = recorder.Recent()
	if len(events) != 2 || events[1].Code != "reconnect_transport" || events[1].Fields["direction"] != "egress" || events[1].Fields["packet_type"] != "1" || events[1].Fields["credential"] != "" {
		t.Fatal("underlay metadata missing or private attribute retained")
	}
	record.PC = reflect.ValueOf(TestProcessLoggerRetainsOnlyAuthoredReconnectMetadata).Pointer()
	if err := handler.Handle(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if events := recorder.Recent(); len(events) != 3 || events[2].Code != "unclassified_log" {
		t.Fatal("untrusted producer retained metadata")
	}
}

func TestHopLoggerRetainsObservedTimeAndRejectsPrivateAttributes(t *testing.T) {
	t.Setenv("PAPERBOAT_TERMINAL_HOPS", "1")
	recorder := diagnostics.NewMemoryRecorder()
	handler := processDiagnosticHandler{recorder: recorder, component: "pb"}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "terminal output hop", reflect.ValueOf(diagnostics.TerminalHop).Pointer())
	record.Add("stage", "pty_read", "observed_unix_us", int64(123456), "range_start", 12, "range_end", 23, "credential", "PRIVATE")
	if err := handler.Handle(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	events := recorder.Recent()
	if len(events) != 1 || events[0].Fields["observed_unix_us"] != "123456" || events[0].Fields["range_end"] != "23" || events[0].Fields["credential"] != "" || events[0].Fields["process_id"] == "" {
		t.Fatal("hop timestamp or privacy contract lost")
	}
}

func TestPacketHopMetadataFitsTheExistingRecordBound(t *testing.T) {
	t.Setenv("PAPERBOAT_TERMINAL_HOPS", "1")
	recorder := diagnostics.NewMemoryRecorder()
	handler := processDiagnosticHandler{recorder: recorder, component: "pb"}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "peer QUIC wire", reflect.ValueOf((*peerquic.Session).Close).Pointer())
	record.Add("trace_id", "trace", "quic_id", "wire", "event", "sent", "packet_number", 1, "datagram_checksum", 123, "stage", "stream_frame", "stream_id", 4, "stream_offset", 23, "stream_length", 17)
	if err := handler.Handle(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	events := recorder.Recent()
	if len(events) != 1 || events[0].Fields["process_id"] == "" || events[0].Fields["stream_length"] != "17" {
		t.Fatal("packet hop cannot be correlated to its process")
	}
}
