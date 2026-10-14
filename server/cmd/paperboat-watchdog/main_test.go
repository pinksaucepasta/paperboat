package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunPreservesConfigurationCausesWithSafeMessages(t *testing.T) {
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "true")
	configDir := t.TempDir()
	t.Run("missing file", func(t *testing.T) {
		missingPath := filepath.Join(configDir, "missing-watchdog.json")
		err := run(context.Background(), []string{missingPath}, &strings.Builder{})
		if err == nil || err.Error() != "watchdog configuration unavailable" {
			t.Fatalf("missing configuration error = %v", err)
		}
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			t.Fatalf("missing configuration lost its filesystem cause: %T", err)
		}
		if strings.Contains(err.Error(), missingPath) {
			t.Fatal("missing configuration exposed its private path")
		}
	})

	t.Run("truncated JSON", func(t *testing.T) {
		truncatedPath := filepath.Join(configDir, "truncated-watchdog.json")
		if err := os.WriteFile(truncatedPath, []byte(`{"targets":[`), 0o600); err != nil {
			t.Fatal("write truncated fixture")
		}
		err := run(context.Background(), []string{truncatedPath}, &strings.Builder{})
		if err == nil || err.Error() != "invalid watchdog configuration" {
			t.Fatalf("truncated configuration error = %v", err)
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated configuration lost its decoder cause: %T", err)
		}
		if strings.Contains(err.Error(), truncatedPath) || strings.Contains(err.Error(), io.ErrUnexpectedEOF.Error()) {
			t.Fatal("invalid configuration exposed parser or path prose")
		}
	})
}

func TestRunPreservesOutputWriterCauseWithSafeMessage(t *testing.T) {
	monitorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer monitorServer.Close()
	probeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer probeServer.Close()

	configPath := filepath.Join(t.TempDir(), "watchdog.json")
	config, err := json.Marshal(configuration{Targets: []target{{Monitor: "pb-watchdog-writer-test", URL: probeServer.URL + "/readyz"}}})
	if err != nil {
		t.Fatal("encode fixture config")
	}
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal("write fixture config")
	}
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "true")
	t.Setenv("PAPERBOAT_SENTRY_DSN", strings.Replace(monitorServer.URL, "http://", "http://public@", 1)+"/1")
	t.Setenv("PAPERBOAT_SENTRY_RELEASE", "watchdog-test")
	t.Setenv("PAPERBOAT_SENTRY_ENVIRONMENT", "test")

	writeErr := errors.New("private output writer detail")
	err = run(context.Background(), []string{configPath}, failingWriter{err: writeErr})
	if err == nil || err.Error() != "watchdog output unavailable" {
		t.Fatalf("output write error = %v", err)
	}
	if !errors.Is(err, writeErr) {
		t.Fatal("output write failure lost its original cause")
	}
	if strings.Contains(err.Error(), writeErr.Error()) || strings.Contains(err.Error(), configPath) || strings.Contains(err.Error(), probeServer.URL) {
		t.Fatal("output failure exposed private writer or request details")
	}
}

func TestRunObservesProbeCauseAndKeepsCheckInSummary(t *testing.T) {
	var checkInMu sync.Mutex
	var checkIns []map[string]any
	exceptionEvents := 0
	sentryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scanner := bufio.NewScanner(r.Body)
		for scanner.Scan() {
			var item map[string]any
			if json.Unmarshal(scanner.Bytes(), &item) != nil {
				continue
			}
			checkInMu.Lock()
			if item["monitor_slug"] != nil {
				checkIns = append(checkIns, item)
			}
			if item["type"] == "event" {
				exceptionEvents++
			}
			checkInMu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sentryServer.Close()

	probeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("SECRET_PROBE_RESPONSE"))
	}))
	defer probeServer.Close()

	configPath := filepath.Join(t.TempDir(), "watchdog.json")
	config, err := json.Marshal(configuration{Targets: []target{{Monitor: "pb-watchdog-test", URL: probeServer.URL + "/readyz"}}})
	if err != nil {
		t.Fatal("encode fixture config")
	}
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal("write fixture config")
	}
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "true")
	t.Setenv("PAPERBOAT_SENTRY_DSN", strings.Replace(sentryServer.URL, "http://", "http://public@", 1)+"/1")
	t.Setenv("PAPERBOAT_SENTRY_RELEASE", "watchdog-test")
	t.Setenv("PAPERBOAT_SENTRY_ENVIRONMENT", "test")

	var logs strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var output strings.Builder
	if err := run(context.Background(), []string{configPath}, &output); err == nil || err.Error() != "watchdog detected unavailable targets" {
		t.Fatal("failed probe did not preserve the watchdog failure result")
	}
	var summary map[string]int
	if err := json.Unmarshal([]byte(output.String()), &summary); err != nil {
		t.Fatalf("watchdog summary is not one JSON record: %v", err)
	}
	if len(summary) != 2 || summary["checked"] != 1 || summary["failed"] != 1 {
		t.Fatalf("watchdog summary changed: %#v", summary)
	}

	var probeFaults []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(logs.String()))
	for scanner.Scan() {
		var entry map[string]any
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry["code"] == "control_unavailable" {
			probeFaults = append(probeFaults, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal("read captured log records")
	}
	if len(probeFaults) != 1 {
		t.Fatalf("expected one local probe observation, got %d records", len(probeFaults))
	}
	fault := probeFaults[0]
	for key, want := range map[string]string{
		"operation": "service_watchdog",
		"stage":     "service_error",
		"code":      "control_unavailable",
		"cause":     "service_unavailable",
		"outcome":   "failed",
		"severity":  "warning",
	} {
		if got, _ := fault[key].(string); got != want {
			t.Errorf("probe diagnostic %s = %q, want %q", key, got, want)
		}
	}
	if got, _ := fault["http_status"].(float64); got != http.StatusServiceUnavailable {
		t.Errorf("probe diagnostic http_status = %v", fault["http_status"])
	}
	if got, _ := fault["support_reference"].(string); got == "" {
		t.Error("probe diagnostic omitted its support reference")
	}
	if strings.Contains(logs.String(), probeServer.URL) || strings.Contains(logs.String(), "SECRET_PROBE_RESPONSE") {
		t.Fatal("probe diagnostic exposed its target or response body")
	}

	checkInMu.Lock()
	defer checkInMu.Unlock()
	if len(checkIns) != 2 || checkIns[0]["status"] != "in_progress" || checkIns[1]["status"] != "error" || exceptionEvents != 0 {
		t.Fatalf("probe added a duplicate exception or changed monitor check-ins: check-ins=%#v exceptions=%d", checkIns, exceptionEvents)
	}
}
