package jobmonitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

func TestActualSDKCheckInLifecycle(t *testing.T) {
	for _, ok := range []bool{true, false} {
		t.Run(map[bool]string{true: "recovered", false: "failed"}[ok], func(t *testing.T) {
			var mu sync.Mutex
			var events []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				scanner := bufio.NewScanner(r.Body)
				for scanner.Scan() {
					var e map[string]any
					if json.Unmarshal(scanner.Bytes(), &e) == nil && e["monitor_slug"] != nil {
						mu.Lock()
						events = append(events, e)
						mu.Unlock()
					}
				}
				w.WriteHeader(200)
			}))
			defer server.Close()
			m, err := New(Config{Enabled: true, DSN: strings.Replace(server.URL, "http://", "http://public@", 1) + "/1", Release: "paperboat-server:test", Environment: "test", Slug: "pb-test-backup"})
			if err != nil {
				t.Fatal(err)
			}
			finish := m.Start()
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() { defer wg.Done(); finish(ok) }()
			}
			wg.Wait()
			m.Close()
			mu.Lock()
			defer mu.Unlock()
			if len(events) != 2 {
				t.Fatalf("expected start and one completion, got %d", len(events))
			}
			want := "error"
			if ok {
				want = "ok"
			}
			if events[0]["status"] != "in_progress" || events[1]["status"] != want || events[0]["check_in_id"] != events[1]["check_in_id"] {
				t.Fatalf("incorrect check-in lifecycle: %#v", events)
			}
			for _, e := range events {
				for key := range e {
					switch key {
					case "check_in_id", "monitor_slug", "status", "duration", "release", "environment":
					default:
						t.Fatalf("unexpected field %q", key)
					}
				}
			}
		})
	}
}

func TestMonitorDisabledAndInvalid(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()(true)
	m.Close()
	for _, c := range []Config{{Enabled: true}, {Enabled: true, DSN: "bad", Slug: "pb-test", Release: "test", Environment: "test"}, {Enabled: true, DSN: "https://public@example.com/1", Slug: "private/path", Release: "test", Environment: "test"}} {
		if _, err := New(c); err == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
}

func TestDeliveryFailureAndDrainStatusAreLocallyVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	m, err := New(Config{Enabled: true, DSN: strings.Replace(server.URL, "http://", "http://public@", 1) + "/1", Release: "server-test", Environment: "test", Slug: "pb-test-backup", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	finish := m.Start()
	finish(false)
	m.Close()
	if m.httpFailures.Load() == 0 || m.FlushStatus() != "drained" {
		t.Fatalf("HTTP failure and SDK drain were conflated or hidden: failures=%d flush=%s", m.httpFailures.Load(), m.FlushStatus())
	}
	for _, expected := range []string{`"cause":"service_unavailable"`, `"http_status":503`, `"flush_status":"drained"`, `"sdk_http_failures":`, `"delivery":"unconfirmed"`} {
		if !strings.Contains(logs.String(), expected) {
			t.Errorf("local monitor log omitted %q: %s", expected, logs.String())
		}
	}
	if strings.Contains(logs.String(), server.URL) || strings.Contains(logs.String(), "public@") {
		t.Fatal("monitor logs exposed the Sentry destination")
	}
}

func TestCaptureFailureUsesBoundedFaultAndRejectsOrdinaryEvents(t *testing.T) {
	var mu sync.Mutex
	var faultEvents []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scanner := bufio.NewScanner(r.Body)
		wantEvent := false
		for scanner.Scan() {
			var item map[string]any
			if wantEvent {
				if json.Unmarshal(scanner.Bytes(), &item) == nil {
					mu.Lock()
					faultEvents = append(faultEvents, item)
					mu.Unlock()
				}
				wantEvent = false
				continue
			}
			if json.Unmarshal(scanner.Bytes(), &item) == nil && item["type"] == "event" {
				wantEvent = true
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	m, err := New(Config{Enabled: true, DSN: strings.Replace(server.URL, "http://", "http://public@", 1) + "/1", Release: "server-test", Environment: "test", Slug: "pb-test-backup", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	secret := errors.New("PRIVATE database host and command output")
	fault := m.CaptureFailure("backup_failed", secret)
	if fault.SupportReference == "" || fault.Cause == "none" || fault.Outcome != "failed" || fault.Severity != "error" {
		t.Fatalf("failure projection is incomplete: %+v", fault)
	}
	if got := m.client.CaptureEvent(&sentry.Event{Message: "PRIVATE arbitrary SDK event"}, nil, nil); got != nil {
		t.Fatal("ordinary SDK event bypassed the fault event admission gate")
	}
	m.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(faultEvents) != 1 {
		t.Fatalf("expected one projected exception event, got %d", len(faultEvents))
	}
	event := faultEvents[0]
	tags, _ := event["tags"].(map[string]any)
	contexts, _ := event["contexts"].(map[string]any)
	faultContext, _ := contexts["fault"].(map[string]any)
	if tags["operation"] != "backup_operation" || tags["stage"] != "backup_execution" || tags["code"] != "service_failed" || tags["support_reference"] != fault.SupportReference || faultContext["outcome"] != "failed" {
		t.Fatalf("exception omitted safe fault identity: %#v", event)
	}
	if _, exists := tags[backupFaultMarker]; exists {
		t.Fatalf("private admission marker escaped sanitizer: %#v", tags)
	}
	if strings.Contains(logs.String(), "PRIVATE") || strings.Contains(server.URL, "PRIVATE") {
		t.Fatal("local or remote diagnostics exposed the wrapped message")
	}
}

func TestCaptureFailureStillWritesLocalEvidenceWhenDisabled(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	m, err := New(Config{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	fault := m.CaptureFailure("report_failed", errors.New("PRIVATE report endpoint"))
	if fault.SupportReference == "" || !strings.Contains(logs.String(), `"operation":"backup_operation"`) || !strings.Contains(logs.String(), `"backup_failure_code":"report_failed"`) || strings.Contains(logs.String(), "PRIVATE") {
		t.Fatalf("disabled monitor did not retain safe local evidence: fault=%+v logs=%s", fault, logs.String())
	}
}

func TestProbeFailureCancellationRedirectAndRecovery(t *testing.T) {
	var mu sync.Mutex
	status := 503
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		s := status
		mu.Unlock()
		if s == 302 {
			w.Header().Set("Location", "/readyz")
		}
		w.WriteHeader(s)
	}))
	defer server.Close()
	err := Probe(context.Background(), server.URL+"/readyz", time.Second)
	if err == nil {
		t.Fatal("unavailable target healthy")
	}
	if fault := projectProbeFault(err); fault.Cause != "service_unavailable" || fault.HTTPStatus != http.StatusServiceUnavailable || fault.Outcome != "failed" {
		t.Fatalf("probe status cause was not preserved as a failure: %+v", fault)
	}
	mu.Lock()
	status = 302
	mu.Unlock()
	if err := Probe(context.Background(), server.URL+"/readyz", time.Second); err == nil {
		t.Fatal("redirect accepted")
	}
	mu.Lock()
	status = 200
	mu.Unlock()
	if err := Probe(context.Background(), server.URL+"/readyz", time.Second); err != nil {
		t.Fatal("recovery not detected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = Probe(ctx, server.URL+"/readyz", time.Second)
	if err == nil || projectProbeFault(err).Cause != "context_canceled" {
		t.Fatal("canceled request healthy")
	}
	for _, u := range []string{server.URL + "/private", server.URL + "/readyz?secret=value", "http://example.com/readyz", "https://user:password@example.com/readyz"} {
		if err := Probe(context.Background(), u, time.Second); err == nil {
			t.Fatal("unsafe endpoint accepted")
		}
	}
}

func TestProbeDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	start := time.Now()
	err := Probe(context.Background(), server.URL+"/readyz", 20*time.Millisecond)
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("probe deadline not respected")
	}
	if fault := projectProbeFault(err); fault.Cause != "deadline_exceeded" || fault.Outcome != "failed" || fault.Severity != "error" {
		t.Fatalf("probe deadline was not retained as an operational failure: %+v", fault)
	}
}

func TestProbePreservesTransportBodyAndCleanupCausesWithoutURLText(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedURL := "http://" + listener.Addr().String() + "/readyz"
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	refused := Probe(context.Background(), refusedURL, time.Second)
	if refused == nil || !errors.Is(refused, syscall.ECONNREFUSED) {
		t.Fatal("connection refusal did not retain its transport cause")
	}
	if fault := projectProbeFault(refused); fault.Cause != "connection_refused" || fault.Errno != int(syscall.ECONNREFUSED) {
		t.Fatalf("connection refusal projection = %+v", fault)
	}
	if strings.Contains(refused.Error(), refusedURL) {
		t.Fatal("probe error exposed its target URL")
	}

	tlsServer := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	tlsServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	tlsServer.StartTLS()
	tlsURL := tlsServer.URL + "/readyz"
	tlsErr := Probe(context.Background(), tlsURL, time.Second)
	tlsServer.Close()
	if tlsErr == nil || projectProbeFault(tlsErr).Cause != "tls_verification" {
		t.Fatalf("TLS trust failure was not retained safely: %+v", projectProbeFault(tlsErr))
	}
	if strings.Contains(tlsErr.Error(), tlsURL) {
		t.Fatal("TLS probe error exposed its target URL")
	}

	bodyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "128")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "SECRET_PARTIAL_RESPONSE")
	}))
	defer bodyServer.Close()
	bodyErr := Probe(context.Background(), bodyServer.URL+"/readyz", time.Second)
	if bodyErr == nil || projectProbeFault(bodyErr).Cause != "truncated_stream" {
		t.Fatalf("partial response body cause was not retained: %+v", projectProbeFault(bodyErr))
	}
	if strings.Contains(bodyErr.Error(), bodyServer.URL) || strings.Contains(bodyErr.Error(), "SECRET_PARTIAL_RESPONSE") {
		t.Fatal("body read failure exposed target or response content")
	}

	closeErr := errors.New("SECRET_BODY_CLOSE")
	responseErr := probeResponse(&http.Response{StatusCode: http.StatusOK, Body: probeTestBody{Reader: strings.NewReader("ok"), closeErr: closeErr}})
	if responseErr == nil || !errors.Is(responseErr, closeErr) || strings.Contains(responseErr.Error(), closeErr.Error()) {
		t.Fatal("body-close failure was lost or formatted")
	}
}

type probeTestBody struct {
	io.Reader
	closeErr error
}

func (b probeTestBody) Close() error { return b.closeErr }

func projectProbeFault(err error) supportreport.Fault {
	return supportreport.ProjectFault(context.Background(), "paperboat-server", "service_watchdog", "service_error", "control_unavailable", err)
}
