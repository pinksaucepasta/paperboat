package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat-server/internal/telemetry"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"
)

type reportingTestStore struct{}

func (reportingTestStore) Save(context.Context, supportreport.Record) error { return nil }
func (reportingTestStore) Lookup(context.Context, string, string, string, bool) ([]supportreport.Record, error) {
	return nil, nil
}
func (reportingTestStore) Cleanup(context.Context) error { return nil }

type reportingCaptureStore struct{ records chan supportreport.Record }

type reportingPrivateError struct {
	cause error
	calls int
}

func (e *reportingPrivateError) Error() string {
	e.calls++
	return "SECRET lifecycle error"
}
func (e *reportingPrivateError) Unwrap() error { return e.cause }

func (s reportingCaptureStore) Save(ctx context.Context, record supportreport.Record) error {
	select {
	case s.records <- record:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (reportingCaptureStore) Lookup(context.Context, string, string, string, bool) ([]supportreport.Record, error) {
	return nil, nil
}
func (reportingCaptureStore) Cleanup(context.Context) error { return nil }
func TestObservedWorkerPreservesFailureCancellationAndPanicCleanup(t *testing.T) {
	reporter, err := supportreport.New(reportingTestStore{}, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	expected := errors.New("private original failure")
	worker := observedWorker(reporter, "worker_machines", func(context.Context) error { return expected })
	if !errors.Is(worker(context.Background()), expected) {
		t.Fatal("changed worker error")
	}
	worker = observedWorker(reporter, "worker_machines", func(ctx context.Context) error { return ctx.Err() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(worker(ctx), context.Canceled) {
		t.Fatal("changed cancellation")
	}
	cleaned := false
	worker = observedWorker(reporter, "worker_machines", func(context.Context) error { defer func() { cleaned = true }(); panic("SECRET") })
	if err := worker(context.Background()); err == nil || err.Error() != "background worker panicked" || !cleaned {
		t.Fatalf("panic cleanup failed: %v %v", err, cleaned)
	}
}

func TestObservedWorkerReportsOperationalFailureWhenCancellationRacesWorkerExit(t *testing.T) {
	store := reportingCaptureStore{records: make(chan supportreport.Record, 4)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	reporter.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	reporterCtx, stopReporter := context.WithCancel(t.Context())
	reporterDone := make(chan error, 1)
	go func() { reporterDone <- reporter.Run(reporterCtx) }()
	defer func() {
		stopReporter()
		if err := <-reporterDone; err != nil {
			t.Errorf("reporter shutdown: %v", err)
		}
		reporter.Close()
	}()

	ctx, cancel := context.WithCancel(t.Context())
	progress := workers.NewProgress(time.Minute)
	worker := observedWorker(reporter, "worker_machines", func(ctx context.Context) error {
		cancel()
		<-ctx.Done()
		return errors.Join(ctx.Err(), &reportingPrivateError{cause: syscall.EIO})
	}, progress)
	if err := worker(ctx); !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("worker exit lost cancellation or operational cause: %v", err)
	}
	cancel()
	if got := progress.Snapshot(); len(got) != 1 || got[0].State != workers.ProgressFailed {
		t.Fatalf("substantive worker failure was hidden by cancellation: %+v", got)
	}

	select {
	case record := <-store.records:
		if record.Code != "worker_failed" || record.Cause != "system_error" || record.Errno != int(syscall.EIO) || record.Outcome != "failed" || record.Reference == "" {
			t.Fatalf("worker exit was not recorded as one actionable operational failure: %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("worker exit failure was lost when cancellation raced it")
	}
	if strings.Contains(logs.String(), "SECRET lifecycle error") || !strings.Contains(logs.String(), `"cause":"system_error"`) || !strings.Contains(logs.String(), `"errno":5`) {
		t.Fatalf("worker exit log lost safe cause or exposed private text: %s", logs.String())
	}
}

func TestSelfhostReportingContextBindsReporter(t *testing.T) {
	records := make(chan supportreport.Record, 1)
	reporter, err := supportreport.New(reportingCaptureStore{records: records}, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = reporter.Run(ctx)
	}()

	bound := selfhostReportingContext(ctx, reporter)
	fault := supportreport.ObserveFromContext(bound, "paperboat-server", "worker_edge_dns", "reconciliation", "edge_dns_reconcile_failed", syscall.ECONNRESET)
	if fault.Cause != "connection_reset" || !supportreport.ValidReference(fault.SupportReference) {
		t.Fatalf("self-host context did not bind report identity: %+v", fault)
	}
	cancel()
	<-done
	select {
	case record := <-records:
		if record.Code != "edge_dns_reconcile_failed" || record.Cause != "connection_reset" || record.Reference != fault.SupportReference {
			t.Fatalf("bound self-host report lost failure identity: %+v", record)
		}
	default:
		t.Fatal("bound self-host reporter did not persist the observation")
	}
}

func TestObservedPeriodicWorkerReportsRealCyclesAndAbsorbedFailure(t *testing.T) {
	progress := workers.NewProgress(time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan int)
	permit := make(chan struct{})
	cycles := 0
	worker := observedWorker(nil, "worker_machines", periodicPreviewTunnelWorker(time.Millisecond, func(ctx context.Context) error {
		cycles++
		select {
		case started <- cycles:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-permit:
		case <-ctx.Done():
			return ctx.Err()
		}
		if cycles == 2 {
			workers.IterationFailure(ctx, "worker_iteration", "worker_iteration_failed", errors.New("private retryable error"))
		}
		return nil
	}), progress)
	done := make(chan error, 1)
	go func() { done <- worker(ctx) }()
	next := func(want int) {
		t.Helper()
		select {
		case n := <-started:
			if n != want {
				t.Fatal(n)
			}
		case <-time.After(time.Second):
			t.Fatal("iteration did not start")
		}
	}
	next(1)
	s := progress.Snapshot()[0]
	if !s.Warming || !s.LastFinished.IsZero() || s.Healthy {
		t.Fatal("first running cycle falsely healthy", s)
	}
	permit <- struct{}{}
	next(2)
	s = progress.Snapshot()[0]
	if !s.Healthy || s.LastFinished.IsZero() {
		t.Fatal("completed idle work not recorded", s)
	}
	permit <- struct{}{}
	next(3)
	if progress.Snapshot()[0].Healthy {
		t.Fatal("retry start hid previous failure")
	}
	permit <- struct{}{}
	next(4)
	if !progress.Snapshot()[0].Healthy {
		t.Fatal("successful cycle did not recover")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not cancel")
	}
	if progress.Snapshot()[0].State != workers.ProgressCanceled {
		t.Fatal("cancellation falsely healthy")
	}
}

func TestWorkerIterationObserverDeduplicatesFailureClassesAndReportsRecovery(t *testing.T) {
	store := reportingCaptureStore{records: make(chan supportreport.Record, 4)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done; reporter.Close() }()
	reporter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := reporter.Context(t.Context(), supportreport.NewReference())
	observe := workerIterationObserver(reporter, "worker_machines")
	observe(ctx, workers.IterationFault{Stage: "worker_iteration", Code: "worker_iteration_failed", Err: errors.New("PRIVATE_MESSAGE")}, false)
	observe(ctx, workers.IterationFault{Stage: "worker_iteration", Code: "worker_iteration_failed", Err: errors.New("PRIVATE_MESSAGE")}, false)
	observe(ctx, workers.IterationFault{Stage: "worker_iteration", Code: "worker_iteration_failed", Err: syscall.ETIMEDOUT}, false)
	observe(ctx, workers.IterationFault{Stage: "worker_recovery", Code: "worker_recovered"}, true)
	var records []supportreport.Record
	for len(records) < 3 {
		select {
		case record := <-store.records:
			records = append(records, record)
		case <-time.After(time.Second):
			t.Fatalf("expected three failure/recovery transitions, got %+v", records)
		}
	}
	if records[0].Code != "worker_iteration_failed" || records[0].Cause != "internal" || records[0].ErrorType == "" || strings.Contains(records[0].ErrorType, "PRIVATE_MESSAGE") {
		t.Fatalf("unsafe or unhelpful first failure projection: %+v", records[0])
	}
	if records[1].Cause != "network_timeout" || records[1].Errno != int(syscall.ETIMEDOUT) {
		t.Fatalf("changed cause not preserved: %+v", records[1])
	}
	if records[2].Outcome != "success" || records[2].Severity != "info" || records[2].Reporting != "disabled" {
		t.Fatalf("recovery was treated as a failure: %+v", records[2])
	}
}

func TestLifecycleFailureUsesSafeLocalCauseAndCorrelation(t *testing.T) {
	store := reportingCaptureStore{records: make(chan supportreport.Record, 2)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	reporter.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
		reporter.Close()
	}()

	private := &reportingPrivateError{cause: syscall.ETIMEDOUT}
	fault := captureLifecycleFailure(reporter, "service_shutdown", "service_failed", private)
	select {
	case record := <-store.records:
		if record.Cause != "network_timeout" || record.Errno != int(syscall.ETIMEDOUT) || record.CorrelationID != "cor_server_lifecycle" || !supportreport.ValidReference(record.Reference) {
			t.Fatalf("lifecycle record lost actionable metadata: %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle failure was not persisted")
	}
	if private.calls != 0 || fault.Cause != "network_timeout" || strings.Contains(logs.String(), "SECRET") {
		t.Fatalf("lifecycle reporter formatted a private error: calls=%d fault=%+v logs=%s", private.calls, fault, logs.String())
	}
	if !strings.Contains(logs.String(), `"correlation_id":"cor_server_lifecycle"`) {
		t.Fatalf("lifecycle log omitted its correlation: %s", logs.String())
	}
}

func TestTelemetryObserverDrainsBeforeReporterAdmissionCloses(t *testing.T) {
	envelopes := make(chan string, 4)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		envelopes <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()
	reporter, err := supportreport.New(reportingTestStore{}, supportreport.Config{
		Enabled: true, DSN: strings.Replace(sink.URL, "://", "://public@", 1) + "/1", Release: "server-test", Environment: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	reporter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancelReporter := context.WithCancel(t.Context())
	reporterDone := make(chan struct{})
	go func() { _ = reporter.Run(runCtx); close(reporterDone) }()
	defer func() { cancelReporter(); <-reporterDone; reporter.Close() }()

	events, err := telemetry.NewEventLog(8)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	events.SetObserver(func(event telemetry.Event) {
		close(entered)
		<-release
		reporter.ObserveEvent(event)
	})
	ref := supportreport.NewReference()
	if _, err := events.Record(telemetry.EventInput{
		At: time.Now().UTC(), Severity: telemetry.SeverityInfo, Component: telemetry.DimensionService,
		Name: "http_billing", Code: "shutdown_probe", Outcome: telemetry.OutcomeSuccess,
		Message: "Operation completed.", CorrelationID: ref, Retry: telemetry.RetryNone,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("telemetry observer did not start")
	}

	stopped := make(chan struct{})
	go func() {
		stopReportingAfterTelemetryDrain(events, func() {
			cancelReporter()
			<-reporterDone
		})
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("reporter stopped while an accepted telemetry event was still pending")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("telemetry and reporter did not drain")
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case envelope := <-envelopes:
			if !strings.Contains(envelope, `"type":"log"`) {
				continue
			}
			if !strings.Contains(envelope, "http_billing") || !strings.Contains(envelope, ref) {
				t.Fatalf("drained operation telemetry missing from Sentry envelope: %s", envelope)
			}
			return
		case <-deadline:
			t.Fatal("accepted operation telemetry was not exported before reporter shutdown")
		}
	}
}
