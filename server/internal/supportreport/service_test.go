package supportreport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
)

type memoryStore struct {
	records   []Record
	fail      bool
	saveErr   error
	lookupErr error
}

func (m *memoryStore) Save(_ context.Context, r Record) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	if m.fail {
		return errors.New("unavailable")
	}
	m.records = append(m.records, r)
	return nil
}

func TestStoreWriteFailurePreservesSafeCauseLocallyAndReportsRecoveryOnce(t *testing.T) {
	privateCause := &privateDiagnosticError{cause: syscall.ETIMEDOUT}
	store := &memoryStore{saveErr: privateCause}
	s, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var logs strings.Builder
	s.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	record := Record{Reference: NewReference(), CorrelationID: "request_store_42"}
	s.process(context.Background(), item{record: record})
	s.process(context.Background(), item{record: record})
	if privateCause.calls != 0 || s.StoreFailures() != 2 || s.Dropped() != 2 || strings.Count(logs.String(), "support report persistence failed") != 1 {
		t.Fatalf("repeated store failure lost cause/count or spammed logs: calls=%d storage=%d dropped=%d logs=%s", privateCause.calls, s.StoreFailures(), s.Dropped(), logs.String())
	}
	for _, expected := range []string{`"cause":"network_timeout"`, `"correlation_id":"request_store_42"`, `"support_reference":"` + record.Reference} {
		if !strings.Contains(logs.String(), expected) {
			t.Errorf("safe store failure log omitted %q: %s", expected, logs.String())
		}
	}
	if strings.Contains(logs.String(), "SECRET") {
		t.Fatal("store failure log exposed arbitrary error text")
	}
	store.saveErr = nil
	s.process(context.Background(), item{record: record})
	if strings.Count(logs.String(), "support report persistence recovered") != 1 || s.StoreFailures() != 2 {
		t.Fatalf("store recovery transition/count incorrect: failures=%d logs=%s", s.StoreFailures(), logs.String())
	}
	status := s.SignalStatus()
	if status["storage_failures"] != uint64(2) {
		t.Fatalf("diagnostics omitted storage failure count: %+v", status)
	}
}

func TestObserveFailureRetainsCauseLocallyWithoutSubmittingException(t *testing.T) {
	store := &memoryStore{}
	s, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var logs strings.Builder
	s.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	reference := NewReference()
	ctx := serverdiagnostics.WithCorrelationID(s.Context(context.Background(), reference), "correlation_observe_01")
	ctx = WithFailureIdentity(ctx, "user_observe_01", "request_observe_01", reference)
	private := errors.New("PRIVATE_DATABASE_DETAIL")
	fault := ObserveFromContext(ctx, "paperboat-server", "http_tunnels", "service_error", "edge_route_reconcile_failed", errors.Join(syscall.ETIMEDOUT, private))
	if fault.Cause != "network_timeout" || fault.Code != "edge_route_reconcile_failed" || fault.Outcome != "failed" || fault.Severity != "warning" || fault.HTTPStatus != 0 || fault.SupportReference != reference {
		t.Fatalf("unexpected observed fault: %+v", fault)
	}
	value := <-s.queue
	if value.unexpected || value.record.EventID != "" || value.record.HTTPStatus != 0 || value.record.RequestID != "request_observe_01" || value.record.CorrelationID != "correlation_observe_01" {
		t.Fatalf("observation was treated as an exception or lost identity: %+v", value)
	}
	s.process(context.Background(), value)
	if len(store.records) != 1 || store.records[0].Cause != "network_timeout" || store.records[0].Severity != "warning" || store.records[0].Reference != reference {
		t.Fatalf("local support evidence lost observed cause: %+v", store.records)
	}
	encoded, err := json.Marshal(store.records[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "PRIVATE_DATABASE_DETAIL") || strings.Contains(string(encoded), "PRIVATE_DATABASE_DETAIL") {
		t.Fatal("observed failure exposed the error message")
	}

	canceled := ObserveFromContext(ctx, "paperboat-server", "http_tunnels", "service_error", "edge_route_reconcile_failed", context.Canceled)
	if canceled.Cause != "context_canceled" || canceled.Outcome != "canceled" || canceled.Severity != "info" {
		t.Fatalf("canceled follow-up was reported as a failure: %+v", canceled)
	}
	canceledItem := <-s.queue
	if canceledItem.unexpected || canceledItem.record.Outcome != "canceled" || canceledItem.record.Severity != "info" {
		t.Fatalf("canceled follow-up became an exception: %+v", canceledItem)
	}
	s.process(context.Background(), canceledItem)
}

func (m *memoryStore) Lookup(_ context.Context, actor, owner, ref string, operator bool) ([]Record, error) {
	if m.lookupErr != nil {
		return nil, m.lookupErr
	}
	if m.fail {
		return nil, ErrUnavailable
	}
	result := []Record{}
	for _, r := range m.records {
		if r.Reference == ref && r.OwnerID == owner && r.ExpiresAt.After(time.Now()) {
			result = append(result, r)
		}
	}
	return result, nil
}

func TestLookupUnavailablePreservesClassifiedCauseWithoutChangingAPIError(t *testing.T) {
	store := &memoryStore{lookupErr: syscall.ETIMEDOUT}
	s, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.Lookup(context.Background(), "user_a", "user_a", NewReference(), false)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, syscall.ETIMEDOUT) {
		t.Fatalf("lookup failure lost sentinel or wrapped cause: %v", err)
	}
	fault := ProjectFault(context.Background(), "paperboat-server", "http_support", "http_handler", "support_unavailable", WithDiagnosticStatus(http.StatusServiceUnavailable, err))
	if fault.Cause != "network_timeout" || fault.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("lookup failure lost operator cause metadata: %+v", fault)
	}
}
func (m *memoryStore) Cleanup(context.Context) error { return nil }
func TestDisabledCaptureRemainsLocallyQueryable(t *testing.T) {
	store := &memoryStore{}
	s, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	ref := NewReference()
	s.Capture(ref, "user_a", "req_1", "authorize", "internal_error", true, sentry.NewStacktrace())
	s.process(context.Background(), <-s.queue)
	s.Capture(ref, "user_a", "req_2", "retry", "timeout", false, nil)
	s.process(context.Background(), <-s.queue)
	records, err := s.Lookup(context.Background(), "user_a", "user_a", ref, false)
	if err != nil || len(records) != 2 || records[0].Stage != "authorize" || records[0].Reporting != "disabled" || records[0].EventID != "" {
		t.Fatalf("unexpected records: %+v %v", records, err)
	}
	records, err = s.Lookup(context.Background(), "user_b", "user_b", ref, false)
	if err != nil || len(records) != 0 {
		t.Fatal("tenant leak")
	}
	if _, err = s.Lookup(context.Background(), "user_b", "user_a", ref, false); !errors.Is(err, ErrInvalid) {
		t.Fatal("unauthorized lookup")
	}
}

func TestCaptureFailureRetainsSafeCauseWhenSentryIsDisabled(t *testing.T) {
	store := &memoryStore{}
	s, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ref := NewReference()
	ctx, finish := s.Begin(context.Background(), "http_previews", ref, "")
	finish("failed")
	ctx = WithFailureIdentity(ctx, "user_a", "request_42")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "request_42")
	wrapped := &privateDiagnosticError{cause: syscall.ETIMEDOUT}
	fault := CaptureFromContext(ctx, "paperboat-server", "http_previews", "http_handler", "billing_unavailable", WithDiagnosticStatus(http.StatusServiceUnavailable, wrapped))
	if wrapped.calls != 0 || fault.SupportReference != ref || fault.CorrelationID != "request_42" || fault.Cause != "network_timeout" {
		t.Fatalf("capture lost safe cause/correlation or formatted error: %+v, calls=%d", fault, wrapped.calls)
	}
	queued := <-s.queue
	if !queued.unexpected || queued.record.Reporting != "disabled" || queued.record.Cause != "network_timeout" || queued.record.ErrorType != fault.ErrorType || queued.record.Errno != int(syscall.ETIMEDOUT) || queued.record.Severity != "error" {
		t.Fatalf("disabled Sentry lost local diagnostic metadata: %+v", queued)
	}
	s.process(context.Background(), queued)
	if len(store.records) != 1 || store.records[0].CorrelationID != "request_42" || store.records[0].Outcome != "failed" || store.records[0].Severity != "error" || store.records[0].HTTPStatus != 503 {
		t.Fatalf("local support record omitted fault metadata: %+v", store.records)
	}
}

func TestExpectedRetryable503IsPersistedWithoutSentryException(t *testing.T) {
	store := &memoryStore{}
	s, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, finish := s.Begin(context.Background(), "http_previews", NewReference(), "")
	finish("failed")
	fault := CaptureFromContext(ctx, "paperboat-server", "http_previews", "http_handler", "edge_admission_unavailable", WithExpectedRetryableStatus(http.StatusServiceUnavailable, syscall.ETIMEDOUT))
	queued := <-s.queue
	if fault.Cause != "network_timeout" || fault.HTTPStatus != http.StatusServiceUnavailable || fault.Severity != "warning" {
		t.Fatalf("expected retryable response lost diagnostic metadata: %+v", fault)
	}
	if queued.unexpected || queued.record.Reporting != "disabled" || queued.record.Cause != "network_timeout" || queued.record.Severity != "warning" {
		t.Fatalf("expected retryable response was queued as an exception or lost locally: %+v", queued)
	}
	s.process(context.Background(), queued)
	if len(store.records) != 1 || store.records[0].HTTPStatus != http.StatusServiceUnavailable || store.records[0].Cause != "network_timeout" {
		t.Fatalf("expected retryable response was not persisted: %+v", store.records)
	}
}

func TestSentryDeliveryFailureIsVisibleAndFlushDoesNotClaimReceipt(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var logs strings.Builder
	s, err := New(&memoryStore{}, Config{Enabled: true, DSN: strings.Replace(server.URL, "://", "://public@", 1) + "/1", Release: "server-test", Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	s.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx := s.Context(context.Background(), NewReference())
	s.CaptureFailure(ctx, "paperboat-server", "http_billing", "http_handler", "synthetic_failure", errors.New("PRIVATE_CAUSE"))
	s.process(context.Background(), <-s.queue)
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	s.close(shutdown)
	cancel()
	status := s.SignalStatus()
	if requests.Load() != 1 || s.SDKHTTPFailures() != 1 || s.FlushStatus() != "drained" || status["delivery"] != "unconfirmed" {
		t.Fatalf("delivery and flush states were conflated: requests=%d status=%+v", requests.Load(), status)
	}
	if status["sdk_submissions_dropped"] != uint64(0) {
		t.Fatalf("an attempted failed delivery was counted as a dropped submission: %+v", status)
	}
	logged := logs.String()
	for _, expected := range []string{`"cause":"service_unavailable"`, `"http_status":503`, "Sentry delivery failed", `"flush_status":"drained"`, `"sdk_http_failures":1`, `"delivery":"unconfirmed"`} {
		if !strings.Contains(logged, expected) {
			t.Errorf("local transport failure log omitted %q: %s", expected, logged)
		}
	}
	if strings.Contains(logged, "PRIVATE_CAUSE") || strings.Contains(logged, server.URL) {
		t.Fatal("local transport failure log exposed error text or destination URL")
	}
}

func TestSentryFlushStatusReportsDeadlineWithoutBlockingShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		close(completed)
	}))
	defer server.Close()
	s, err := New(&memoryStore{}, Config{Enabled: true, DSN: strings.Replace(server.URL, "://", "://public@", 1) + "/1", Release: "server-test", Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	s.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	s.CaptureFailure(s.Context(context.Background(), NewReference()), "paperboat-server", "http_billing", "http_handler", "synthetic_failure", errors.New("private"))
	s.process(context.Background(), <-s.queue)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Sentry request did not start")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	begin := time.Now()
	s.close(shutdown)
	cancel()
	if time.Since(begin) > time.Second || s.FlushStatus() != "timed_out" {
		t.Fatalf("shutdown ignored its flush deadline: elapsed=%s status=%s", time.Since(begin), s.FlushStatus())
	}
	for _, expected := range []string{`"flush_status":"timed_out"`, `"delivery":"unconfirmed"`} {
		if !strings.Contains(logs.String(), expected) {
			t.Errorf("bounded local shutdown log omitted %q: %s", expected, logs.String())
		}
	}
	close(release)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("in-flight Sentry test request did not finish after release")
	}
}

func TestReporterCloseFencesNewFailureSubmissions(t *testing.T) {
	s, err := New(&memoryStore{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := s.Context(context.Background(), NewReference())
	s.Close()
	fault := s.CaptureFailure(ctx, "paperboat-server", "http_billing", "http_handler", "synthetic_failure", errors.New("private"))
	if fault.Cause != "internal" || len(s.queue) != 0 || s.SDKSubmissionsDropped() != 1 {
		t.Fatalf("closed reporter admitted or silently lost a failure: fault=%+v queued=%d dropped=%d", fault, len(s.queue), s.SDKSubmissionsDropped())
	}
}

func TestCaptureFromContextWithoutReporterLogsSafeProjectionAndSource(t *testing.T) {
	var output strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	ctx := serverdiagnostics.WithCorrelationID(context.Background(), "request_fallback_42")
	ctx = WithFailureIdentity(ctx, "user_a", "request_fallback_42", NewReference())
	err := &privateDiagnosticError{cause: syscall.ETIMEDOUT}
	fault := CaptureFromContext(ctx, "paperboat-server", "http_previews", "http_handler", "internal_error", WithDiagnosticStatus(http.StatusServiceUnavailable, err))
	if err.calls != 0 || fault.Cause != "network_timeout" || fault.HTTPStatus != http.StatusServiceUnavailable || fault.CorrelationID != "request_fallback_42" {
		t.Fatalf("local fallback lost safe cause/correlation or formatted private error: %+v, calls=%d", fault, err.calls)
	}
	if strings.ContainsAny(fault.SourceFile, "/\\") || len(fault.SourceFile) > 128 || len(fault.SourceFunction) > 64 || fault.SourceLine < 0 || fault.SourceLine > 1_000_000 {
		t.Fatalf("local fallback source identity was not bounded: %+v", fault)
	}
	logged := output.String()
	for _, expected := range []string{`"cause":"network_timeout"`, `"http_status":503`} {
		if !strings.Contains(logged, expected) {
			t.Errorf("safe local log omitted %s: %s", expected, logged)
		}
	}
	if strings.Contains(logged, "SECRET") {
		t.Fatal("local fallback logged private error text")
	}
}

func TestCleanupFailureCountsEveryFailureAndReportsTransitionsOnce(t *testing.T) {
	s, err := New(&memoryStore{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ref := NewReference()
	failed := false
	s.observeCleanup(context.Background(), ref, &failed, errors.New("SECRET cleanup error"))
	s.observeCleanup(context.Background(), ref, &failed, errors.New("SECRET cleanup error"))
	if !failed || s.CleanupFailures() != 2 || len(s.queue) != 1 {
		t.Fatalf("cleanup failure count/transition incorrect: failed=%v count=%d queued=%d", failed, s.CleanupFailures(), len(s.queue))
	}
	s.observeCleanup(context.Background(), ref, &failed, nil)
	if failed || len(s.queue) != 2 {
		t.Fatalf("cleanup recovery was not reported exactly once: failed=%v queued=%d", failed, len(s.queue))
	}
	first := <-s.queue
	recovery := <-s.queue
	if first.record.Cause != "internal" || strings.Contains(first.record.Cause+first.record.ErrorType, "SECRET") || first.record.Code != "support_cleanup_failed" {
		t.Fatalf("unsafe cleanup failure projection: %+v", first.record)
	}
	if recovery.record.Outcome != "success" || recovery.record.Severity != "info" || recovery.unexpected {
		t.Fatalf("cleanup recovery was treated as an error: %+v", recovery)
	}
	if got := s.SignalStatus()["cleanup_failures"]; got != uint64(2) {
		t.Fatalf("disabled reporting hid cleanup failures: %v", got)
	}
}

func TestCleanupDeadlineIsCountedAndReportedAsFailure(t *testing.T) {
	s, err := New(&memoryStore{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	failed := false
	s.observeCleanup(ctx, NewReference(), &failed, context.DeadlineExceeded)
	if !failed || s.CleanupFailures() != 1 || len(s.queue) != 1 {
		t.Fatalf("deadline cleanup failure was suppressed: failed=%v count=%d queued=%d", failed, s.CleanupFailures(), len(s.queue))
	}
	queued := <-s.queue
	if queued.record.Cause != "deadline_exceeded" || queued.record.Outcome != "failed" || queued.record.Severity != "error" || queued.record.Code != "support_cleanup_failed" {
		t.Fatalf("cleanup deadline was not projected as a failure: %+v", queued.record)
	}
}

func TestSentryWirePayloadContainsOnlyApprovedData(t *testing.T) {
	received := make(chan []byte, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		received <- payload
		w.WriteHeader(200)
	}))
	defer server.Close()
	s, err := New(&memoryStore{}, Config{Enabled: true, DSN: strings.Replace(server.URL, "://", "://public@", 1) + "/1", Release: "server-test", Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ref := NewReference()
	s.Capture(ref, "user_secret", "req_1", "authorize", "internal_error", true, &sentry.Stacktrace{Frames: []sentry.Frame{{Function: "Authorize", Module: "github.com/pinksaucepasta/paperboat/server/internal/auth", Filename: "/private/SECRET/auth.go", AbsPath: "SECRET", Vars: map[string]any{"token": "SECRET"}, ContextLine: "SECRET", Lineno: 17}}})
	s.process(context.Background(), <-s.queue)
	if !s.client.Flush(time.Second) {
		t.Fatal("SDK queue did not flush")
	}
	defer s.client.Close()
	payload := <-received
	if len(payload) == 0 || !strings.Contains(string(payload), ref) || !strings.Contains(string(payload), "auth.go") || !strings.Contains(string(payload), "Authorize") {
		t.Fatalf("missing readable evidence: %s", payload)
	}
	for _, secret := range []string{"SECRET", "user_secret", "/private/", "vars", "context_line", "server_name"} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("wire payload leaked %s", secret)
		}
	}
	dirty := &sentry.Event{Tags: map[string]string{"support_reference": ref, "component": "paperboat-server", "stage": "authorize", "code": "internal_error"}, Message: "SECRET", Request: &sentry.Request{URL: "https://SECRET", Headers: map[string]string{"Authorization": "SECRET"}}, User: sentry.User{Email: "SECRET"}, Breadcrumbs: []*sentry.Breadcrumb{{Message: "SECRET"}}, Exception: []sentry.Exception{{Value: "SECRET"}}}
	out, _ := json.Marshal(sanitize(dirty))
	if strings.Contains(string(out), "SECRET") {
		t.Fatal("sanitizer leaked arbitrary event data")
	}
}
func TestCaptureBoundsAndStorageFailure(t *testing.T) {
	s, _ := New(&memoryStore{fail: true}, Config{})
	for i := 0; i < 1000; i++ {
		s.Capture(NewReference(), "user_a", "req_1", "handler", "internal_error", true, nil)
	}
	if len(s.queue) > QueueCapacity || s.Dropped() == 0 {
		t.Fatal("unbounded capture")
	}
	before := s.Dropped()
	s.process(context.Background(), <-s.queue)
	if s.Dropped() != before+1 {
		t.Fatal("lost storage failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	s.Run(ctx)
	if time.Since(start) > time.Second {
		t.Fatal("shutdown blocked")
	}
}

func TestSentryOutageNeverBlocksCaptureAndQueueExhaustion(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	s, err := New(&memoryStore{}, Config{Enabled: true, DSN: strings.Replace(server.URL, "://", "://public@", 1) + "/1", Release: "test", Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	s.Capture(NewReference(), "usr_a", "req_a", "handler", "internal_error", true, nil)
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("Sentry request did not start")
	}
	start := time.Now()
	s.Capture(NewReference(), "usr_a", "req_b", "handler", "internal_error", true, nil)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("capture waited on Sentry")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("outage blocked shutdown")
	}
	local, _ := New(&memoryStore{}, Config{})
	for i := 0; i < QueueCapacity+5; i++ {
		local.window = time.Now().Add(-time.Minute)
		local.Capture(NewReference(), "usr_a", "req_a", "handler", "internal_error", true, nil)
	}
	if len(local.queue) != QueueCapacity || local.Dropped() != 5 {
		t.Fatal("queue capacity not enforced", len(local.queue), local.Dropped())
	}
}

func TestRuntimeStackRetainsSafeSourceFilename(t *testing.T) {
	stack := cleanStack(sentry.NewStacktrace())
	found := false
	for _, frame := range stack.Frames {
		if frame.Filename == "service_test.go" {
			found = true
		}
		if frame.AbsPath != "" || strings.Contains(frame.Filename, "/") {
			t.Fatal("absolute source path escaped")
		}
	}
	if !found {
		t.Fatal("real SDK runtime stack lost source filename")
	}
}

type cancelOnSaveStore struct {
	memoryStore
	cancel context.CancelFunc
}

func (s *cancelOnSaveStore) Save(ctx context.Context, record Record) error {
	s.cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memoryStore.Save(ctx, record)
}

func TestAcceptedReportSurvivesCancellationDuringSave(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store := &cancelOnSaveStore{cancel: cancel}
	reporter, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	fault := reporter.ObserveFailure(context.Background(), "paperboat-server", "worker_edge_dns", "reconciliation", "edge_dns_reconcile_failed", syscall.ECONNRESET)
	done := make(chan error, 1)
	go func() { done <- reporter.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reporter failed to stop after accepted save")
	}
	if len(store.records) != 1 || store.records[0].Reference != fault.SupportReference || store.records[0].Cause != "connection_reset" {
		t.Fatalf("accepted observation lost during cancellation: saved=%d", len(store.records))
	}
}
