package main

import (
	"bufio"
	"bytes"
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

	"github.com/pinksaucepasta/paperboat-server/internal/backupreport"
)

type backupCommandReceipts struct {
	mu              sync.Mutex
	reports         []backupreport.Report
	events          []map[string]any
	checkInStatuses []string
	envelopes       [][]byte
	reportStatus    int
}

func (r *backupCommandReceipts) sentryHandler(w http.ResponseWriter, request *http.Request) {
	raw, _ := io.ReadAll(request.Body)
	r.mu.Lock()
	r.envelopes = append(r.envelopes, append([]byte(nil), raw...))
	r.mu.Unlock()

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	var itemType string
	for scanner.Scan() {
		var header map[string]any
		if json.Unmarshal(scanner.Bytes(), &header) == nil {
			if typ, ok := header["type"].(string); ok {
				itemType = typ
				continue
			}
		}
		line := append([]byte(nil), scanner.Bytes()...)
		switch itemType {
		case "event":
			var event map[string]any
			if json.Unmarshal(line, &event) == nil {
				r.mu.Lock()
				r.events = append(r.events, event)
				r.mu.Unlock()
			}
		case "check_in":
			var checkIn map[string]any
			if json.Unmarshal(line, &checkIn) == nil {
				if status, ok := checkIn["status"].(string); ok {
					r.mu.Lock()
					r.checkInStatuses = append(r.checkInStatuses, status)
					r.mu.Unlock()
				}
			}
		}
		itemType = ""
	}
	w.WriteHeader(http.StatusOK)
}

func (r *backupCommandReceipts) reportHandler(w http.ResponseWriter, request *http.Request) {
	var report backupreport.Report
	if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.reports = append(r.reports, report)
	status := r.reportStatus
	r.mu.Unlock()
	w.WriteHeader(status)
}

func TestBackupCommandProjectsPGDumpFailureAndFailsCheckIn(t *testing.T) {
	receipts, logs, output, err := runBackupCommand(t, true, "pg_dump", http.StatusNoContent, true)
	var producerErr *backupreport.ProducerError
	if !errors.As(err, &producerErr) || producerErr.Code != "backup_failed" || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("backup failure was not returned as its finite producer error: %v", err)
	}
	if !strings.Contains(logs, `"operation":"backup_operation"`) || !strings.Contains(logs, `"backup_failure_code":"backup_failed"`) || strings.Contains(logs, "PRIVATE") {
		t.Fatalf("local final owner log is missing bounded failure evidence or contains provider output: %s", logs)
	}
	if len(receipts.reports) != 1 || receipts.reports[0].Outcome != "failed" || receipts.reports[0].FailureCode != "backup_failed" || !strings.Contains(output, `"outcome":"failed"`) {
		t.Fatalf("durable failed-operation report was not preserved: reports=%+v output=%s", receipts.reports, output)
	}
	if len(receipts.events) != 1 || len(receipts.checkInStatuses) != 2 || receipts.checkInStatuses[0] != "in_progress" || receipts.checkInStatuses[1] != "error" {
		t.Fatalf("expected one exception and failed final check-in, events=%d check-ins=%v", len(receipts.events), receipts.checkInStatuses)
	}
	if message, _ := receipts.events[0]["message"].(string); message != "" {
		t.Fatalf("unexpected free-form exception message: %q", message)
	}
	tags, _ := receipts.events[0]["tags"].(map[string]any)
	if tags["operation"] != "backup_operation" || tags["stage"] != "backup_execution" || tags["support_reference"] == "" || tags["cause"] == "none" {
		t.Fatalf("Sentry event lacks bounded fault cause/reference: %#v", receipts.events[0])
	}
	for _, envelope := range receipts.envelopes {
		if bytes.Contains(envelope, []byte("PRIVATE")) {
			t.Fatal("Sentry envelope exposed command output")
		}
	}
}

func TestBackupCommandFailureStillLogsWhenSentryDisabled(t *testing.T) {
	_, logs, _, err := runBackupCommand(t, false, "pg_dump", http.StatusNoContent, true)
	if err == nil || !strings.Contains(logs, `"cause":"internal"`) || !strings.Contains(logs, `"backup_failure_code":"backup_failed"`) || strings.Contains(logs, "PRIVATE") {
		t.Fatalf("disabled monitoring lost local failure evidence or exposed command output: err=%v logs=%s", err, logs)
	}
}

func TestBackupCommandOutputFailureKeepsProducerAndWriterCauses(t *testing.T) {
	writerCause := errors.New("PRIVATE output sink detail")
	receipts, logs, err := runBackupCommandWithWriter(t, true, "pg_dump", http.StatusNoContent, &failingBackupWriter{err: writerCause})
	var producerErr *backupreport.ProducerError
	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &producerErr) || producerErr.Code != "backup_failed" || !errors.Is(err, writerCause) || !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("output failure discarded a cause or exposed its text: %v", err)
	}
	if len(receipts.reports) != 1 || len(receipts.events) != 1 || !strings.Contains(logs, `"backup_failure_code":"backup_failed"`) || strings.Contains(logs, "PRIVATE") {
		t.Fatalf("final owner did not report joined command/output causes safely: reports=%d events=%d logs=%s", len(receipts.reports), len(receipts.events), logs)
	}
}

func TestReportTokenPreservesFilesystemCauseWithoutPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-token")
	setBackupTestEnv(t, map[string]string{"PAPERBOAT_BACKUP_REPORT_TOKEN": "", "PAPERBOAT_BACKUP_REPORT_TOKEN_FILE": path})
	_, err := reportToken()
	if err == nil || !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), path) || backupFailureCode(err) != "storage_unavailable" {
		t.Fatalf("token file failure lost its cause or exposed its path: err=%v code=%s", err, backupFailureCode(err))
	}
}

func TestCleanupOutputFailureHasNilMonitorLocalOwner(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	setBackupTestEnv(t, map[string]string{
		"PAPERBOAT_BACKUP_REPORT_TOKEN":      strings.Repeat("t", 32),
		"PAPERBOAT_BACKUP_REPORT_TOKEN_FILE": "",
		"PAPERBOAT_SENTRY_ENABLED":           "false",
	})
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	writerCause := errors.New("PRIVATE cleanup output sink")
	err := run(context.Background(), []string{"cleanup", "--storage", filepath.Join(t.TempDir(), "storage"), "--report-url", server.URL + "/v1/operations/backups/reports"}, &failingBackupWriter{err: writerCause})
	var commandErr *backupCommandError
	if !errors.As(err, &commandErr) || commandErr.code != "storage_unavailable" || !errors.Is(err, writerCause) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("cleanup output failure lost its bounded cause: %v", err)
	}
	if !strings.Contains(logs.String(), `"stage":"backup_cleanup"`) || strings.Contains(logs.String(), "PRIVATE") {
		t.Fatalf("nil monitor did not own safe cleanup failure logging: %s", logs.String())
	}
}

func TestBackupCommandReport503KeepsServiceCauseAndHealthyRunCompletesCheckIn(t *testing.T) {
	t.Run("report unavailable", func(t *testing.T) {
		receipts, logs, _, err := runBackupCommand(t, true, "healthy", http.StatusServiceUnavailable, false)
		var producerErr *backupreport.ProducerError
		if !errors.As(err, &producerErr) || producerErr.Code != "report_failed" || !strings.Contains(logs, `"http_status":503`) || !strings.Contains(logs, `"cause":"service_unavailable"`) {
			t.Fatalf("reporting outage lost typed status/cause: err=%v logs=%s", err, logs)
		}
		if len(receipts.events) != 1 || len(receipts.checkInStatuses) != 2 || receipts.checkInStatuses[1] != "error" {
			t.Fatalf("reporting outage did not create one exception and failed check-in: events=%d statuses=%v", len(receipts.events), receipts.checkInStatuses)
		}
	})
	t.Run("healthy", func(t *testing.T) {
		receipts, logs, output, err := runBackupCommand(t, true, "healthy", http.StatusNoContent, false)
		if err != nil || strings.Contains(logs, `"backup operation failed"`) || !strings.Contains(output, `"outcome":"succeeded"`) {
			t.Fatalf("healthy command did not complete cleanly: err=%v logs=%s output=%s", err, logs, output)
		}
		if len(receipts.events) != 0 || len(receipts.reports) != 1 || receipts.reports[0].Outcome != "succeeded" || len(receipts.checkInStatuses) != 2 || receipts.checkInStatuses[1] != "ok" {
			t.Fatalf("healthy command produced incorrect remote evidence: events=%d reports=%+v statuses=%v", len(receipts.events), receipts.reports, receipts.checkInStatuses)
		}
	})
}

func runBackupCommand(t *testing.T, enabled bool, dumpMode string, reportStatus int, privateOutput bool) (*backupCommandReceipts, string, string, error) {
	t.Helper()
	var output bytes.Buffer
	receipts, logs, err := runBackupCommandWithWriter(t, enabled, dumpMode, reportStatus, &output)
	if privateOutput && strings.Contains(output.String(), "PRIVATE") {
		t.Fatal("command output exposed provider text")
	}
	return receipts, logs, output.String(), err
}

type failingBackupWriter struct{ err error }

func (w *failingBackupWriter) Write([]byte) (int, error) { return 0, w.err }

func runBackupCommandWithWriter(t *testing.T, enabled bool, dumpMode string, reportStatus int, output io.Writer) (*backupCommandReceipts, string, error) {
	t.Helper()
	receipts := &backupCommandReceipts{reportStatus: reportStatus}
	sentryServer := httptest.NewServer(http.HandlerFunc(receipts.sentryHandler))
	t.Cleanup(sentryServer.Close)
	reportServer := httptest.NewServer(http.HandlerFunc(receipts.reportHandler))
	t.Cleanup(reportServer.Close)

	temp := t.TempDir()
	bin := filepath.Join(temp, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	dumpScript := "#!/bin/sh\n"
	if dumpMode == "pg_dump" {
		dumpScript += "printf '%s' 'custom archive fixture'\nprintf '%s\\n' 'PRIVATE database host and command output' >&2\nexit 17\n"
	} else {
		dumpScript += "printf '%s' 'custom archive fixture'\nexit 0\n"
	}
	for _, name := range []string{"pg_dump", "pg_restore"} {
		contents := dumpScript
		if name == "pg_restore" {
			contents = "#!/bin/sh\nexit 0\n"
		}
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte(contents), 0700); err != nil {
			t.Fatal(err)
		}
	}
	setBackupTestEnv(t, map[string]string{
		"PATH":                               bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PAPERBOAT_BACKUP_REPORT_TOKEN":      strings.Repeat("t", 32),
		"PAPERBOAT_BACKUP_REPORT_TOKEN_FILE": "",
		"PAPERBOAT_SENTRY_ENABLED":           map[bool]string{true: "true", false: "false"}[enabled],
		"PAPERBOAT_SENTRY_DSN":               strings.Replace(sentryServer.URL, "http://", "http://public@", 1) + "/1",
		"PAPERBOAT_SENTRY_RELEASE":           "paperboat-server:test",
		"PAPERBOAT_SENTRY_ENVIRONMENT":       "test",
		"PAPERBOAT_SENTRY_BACKUP_MONITOR":    "pb-test-backup",
	})
	var logBuffer bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	args := []string{"backup", "--database", "primary", "--source-service", "backup_source", "--storage", filepath.Join(temp, "storage"), "--report-url", reportServer.URL + "/v1/operations/backups/reports", "--retention", "1h"}
	err := run(context.Background(), args, output)
	return receipts, logBuffer.String(), err
}

func setBackupTestEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for key, value := range values {
		old, present := os.LookupEnv(key)
		if err := os.Setenv(key, value); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}
