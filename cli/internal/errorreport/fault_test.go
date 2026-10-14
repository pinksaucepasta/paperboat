package errorreport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type statusFault int

func (s statusFault) Error() string         { panic("raw status error must not be inspected") }
func (s statusFault) DiagnosticStatus() int { return int(s) }

type cyclicFault struct{}

func (*cyclicFault) Error() string   { panic("raw cyclic error must not be inspected") }
func (e *cyclicFault) Unwrap() error { return e }

type producerFault struct {
	stage, code string
	cause       error
}

func (e producerFault) Error() string           { panic("producer error text must not be inspected") }
func (e producerFault) Unwrap() error           { return e.cause }
func (e producerFault) DiagnosticStage() string { return e.stage }
func (e producerFault) DiagnosticCode() string  { return e.code }

func TestFaultKeepsProducerPhaseAndCodeWithoutAcceptingPrivateLabels(t *testing.T) {
	err := producerFault{stage: "delivery", code: "file_transfer_failed", cause: context.DeadlineExceeded}
	fault := ProjectFault(t.Context(), "pb", "send", "command", "unexpected_cli_failure", err)
	if fault.Stage != "delivery" || fault.Code != "file_transfer_failed" || fault.Cause != "deadline_exceeded" {
		t.Fatalf("producer phase was discarded: %#v", fault)
	}
	err.stage, err.code = "private_stage", "private_code"
	fault = ProjectFault(t.Context(), "pb", "send", "command", "unexpected_cli_failure", err)
	if fault.Stage != "command" || fault.Code != "unexpected_cli_failure" {
		t.Fatal("unowned producer metadata crossed the diagnostic boundary")
	}
}

func TestFaultDistinguishesCausesWithoutErrorText(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		cause string
	}{
		{"canceled", fmt.Errorf("PRIVATE: %w", context.Canceled), "context_canceled"},
		{"deadline", fmt.Errorf("PRIVATE: %w", context.DeadlineExceeded), "deadline_exceeded"},
		{"permission", &os.PathError{Op: "PRIVATE", Path: "PRIVATE", Err: syscall.EACCES}, "permission_denied"},
		{"refused", &net.OpError{Op: "PRIVATE", Err: syscall.ECONNREFUSED}, "connection_refused"},
		{"dns", &net.DNSError{Name: "PRIVATE", Err: "PRIVATE"}, "dns_failure"},
		{"conflict", statusFault(409), "conflict"},
		{"rate", statusFault(429), "rate_limited"},
		{"unavailable", statusFault(503), "service_unavailable"},
		{"upstream", statusFault(500), "upstream_failure"},
		{"cycle", &cyclicFault{}, "internal"},
		{"join", errors.Join(syscall.ECONNREFUSED, context.Canceled), "connection_refused"},
		{"joined deadline", errors.Join(context.Canceled, context.DeadlineExceeded), "deadline_exceeded"},
		{"joined unknown", errors.Join(context.Canceled, errors.New("PRIVATE_PAYLOAD")), "internal"},
		{"joined cycle", errors.Join(context.Canceled, &cyclicFault{}), "internal"},
		{"joined typed nil", errors.Join(context.Canceled, (*cyclicFault)(nil)), "internal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := ProjectFault(context.Background(), "pb", "connect", "peer_connect", "transport_failed", test.err)
			if f.Cause != test.cause || !supportref.Valid(f.SupportReference) || len(f.ErrorChain) > 8 {
				t.Fatalf("fault=%+v", f)
			}
			encoded, _ := json.Marshal(f)
			if strings.Contains(string(encoded), "PRIVATE") {
				t.Fatal("raw error content leaked")
			}
		})
	}
}

func TestFaultCancellationRequiresCompleteBoundedEvidence(t *testing.T) {
	err := error(context.Canceled)
	for range 20 {
		err = fmt.Errorf("PRIVATE: %w", err)
	}
	fault := ProjectFault(t.Context(), "pb", "command", "command", "command_failed", errors.Join(context.Canceled, err))
	if fault.Cause != "internal" || fault.Outcome != "failed" {
		t.Fatal("unresolved cancellation chain was reported as orderly")
	}
	shared := fmt.Errorf("PRIVATE: %w", context.Canceled)
	fault = ProjectFault(t.Context(), "pb", "command", "command", "command_failed", errors.Join(shared, shared))
	if fault.Cause != "context_canceled" || fault.Outcome != "canceled" {
		t.Fatal("shared cancellation branches were mistaken for a failure")
	}
}

func TestFaultLocalEvidenceSurvivesDisabledExportAndQuota(t *testing.T) {
	var observed []Fault
	restore := InstallFaultObserver(func(_ context.Context, f Fault) { observed = append(observed, f); f.ErrorChain[0] = "changed" })
	defer restore()
	var disabled *Reporter
	f := disabled.CaptureFailure(context.Background(), "pb", "connect", "peer_connect", "transport_failed", syscall.ECONNREFUSED)
	if len(observed) != 1 || f.ErrorChain[0] == "changed" {
		t.Fatal("local fault missing or shared mutable chain")
	}
	transport := &recordingTransport{}
	r := newReporter("https://public@example.invalid/1", "test", transport)
	defer r.Flush(context.Background())
	ctx := supportref.WithContext(context.Background(), f.SupportReference)
	for range maximumPerMinute + 2 {
		r.CaptureFailure(ctx, "pb", "connect", "peer_connect", "transport_failed", syscall.ECONNREFUSED)
	}
	if len(observed) != maximumPerMinute+3 || len(transport.events) != maximumPerMinute {
		t.Fatal("Sentry quota affected independent local evidence")
	}
	for _, event := range transport.events {
		if event.Tags["cause"] != "connection_refused" || event.Tags["support_reference"] != f.SupportReference || event.Tags["errno"] == "" || event.Tags["error_chain"] == "" || event.Exception[0].Value != "connection_refused" {
			t.Fatalf("fault metadata missing: %+v", event)
		}
		if event.Tags["source_file"] != "fault_test.go" || !strings.HasSuffix(event.Tags["source_function"], ".TestFaultLocalEvidenceSurvivesDisabledExportAndQuota") || event.Tags["source_line"] == "" {
			t.Fatal("safe source metadata missing")
		}
	}
}

func TestFaultCancellationIsLocalInfoAndLifecycleCapturesOnce(t *testing.T) {
	var observed []Fault
	restore := InstallFaultObserver(func(_ context.Context, f Fault) { observed = append(observed, f) })
	defer restore()
	transport := &recordingTransport{}
	r := newReporter("https://public@example.invalid/1", "test", transport)
	defer r.Flush(context.Background())
	r.CaptureFailure(context.Background(), "pb", "connect", "peer_connect", "transport_failed", context.Canceled)
	if len(transport.events) != 0 || observed[0].Severity != "info" || observed[0].Outcome != "canceled" {
		t.Fatal("expected cancellation captured as exception")
	}
	r.ServiceLifecycle(context.Background(), "peer_transport", "component_start", syscall.ECONNREFUSED)
	if len(transport.events) != 1 || len(observed) != 2 || observed[1].Cause != "connection_refused" {
		t.Fatal("service fault duplicated or cause discarded")
	}
	(&Reporter{}).ServiceLifecycle(context.Background(), "peer_transport", "component_start", syscall.ECONNREFUSED)
	if len(observed) != 3 {
		t.Fatal("disabled Sentry suppressed lifecycle local evidence")
	}
}

func TestFaultSanitizerRejectsUnownedLabelsAndRetainsSafeWireMetadata(t *testing.T) {
	f := ProjectFault(context.Background(), "pb", "connect", "secret_stage", "secret_code", &cyclicFault{})
	if f.Stage != "unknown" || f.Code != "internal_failure" {
		t.Fatal("unowned classification became telemetry label")
	}
	transport := &sentry.MockTransport{}
	r := newSignalReporter("https://public@example.invalid/1", "test", transport, true, false, 0)
	r.CaptureFailure(context.Background(), "pb", "connect", "peer_connect", "transport_failed", statusFault(503))
	r.Flush(context.Background())
	found := false
	for _, event := range transport.Events() {
		for _, log := range event.Logs {
			if log.Level == sentry.LogLevelError && log.Attributes["stage"].AsString() == "peer_connect" && log.Attributes["cause"].AsString() == "service_unavailable" && log.Attributes["error_type"].AsString() == "statusFault" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("safe fault log metadata missing")
	}
	// Flushing the disabled exporter remains a bounded no-op.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	(&Reporter{}).Flush(ctx)
}

func TestFaultLocalEvidenceSurvivesBlockedSentryQueue(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(503)
	}))
	defer server.Close()
	defer close(release)
	transport := sentry.NewHTTPTransport()
	transport.BufferSize = 1
	transport.Timeout = 100 * time.Millisecond
	r := newReporter(strings.Replace(server.URL, "http://", "http://public@", 1)+"/1", "test", transport)
	var local atomic.Uint64
	restore := InstallFaultObserver(func(context.Context, Fault) { local.Add(1) })
	defer restore()
	r.CaptureFailure(context.Background(), "pb", "connect", "peer_connect", "transport_failed", syscall.ECONNREFUSED)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Sentry transport did not start")
	}
	started := time.Now()
	for range 20 {
		r.CaptureFailure(context.Background(), "pb", "connect", "peer_connect", "transport_failed", syscall.ECONNREFUSED)
	}
	if local.Load() != 21 || time.Since(started) > time.Second {
		t.Fatal("blocked export suppressed or blocked local fault capture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r.Flush(ctx)
}

func TestDisabledLifecyclePreservesProcessReferenceAndCyclicCause(t *testing.T) {
	reference := supportref.New()
	r := &Reporter{}
	_, finish := r.Start(supportref.WithContext(context.Background(), reference), "paperboat-daemon", "daemon")
	defer finish("success")
	var observed Fault
	restore := InstallFaultObserver(func(_ context.Context, f Fault) { observed = f })
	defer restore()
	r.ServiceLifecycle(context.Background(), "peer_transport", "component_start", &cyclicFault{})
	if observed.SupportReference != reference || observed.Cause != "internal" || len(observed.ErrorChain) != 1 {
		t.Fatal("disabled lifecycle lost reference or failed bounded cycle handling")
	}
}

func TestOwnedTransportRecordsDisabledAttemptWithoutChangingContract(t *testing.T) {
	for _, test := range []struct {
		name           string
		err            error
		status         int
		cause, outcome string
	}{
		{"network", &net.OpError{Op: "PRIVATE", Err: syscall.ECONNREFUSED}, 0, "connection_refused", "failed"},
		{"denied", nil, 403, "permission_denied", "rejected"},
		{"conflict", nil, 409, "conflict", "rejected"},
		{"rate", nil, 429, "rate_limited", "rejected"},
		{"unavailable", nil, 503, "service_unavailable", "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reporter := &Reporter{}
			restore := Install(reporter)
			defer restore()
			var observed Fault
			restoreObserver := InstallFaultObserver(func(_ context.Context, f Fault) { observed = f })
			defer restoreObserver()
			response := &http.Response{StatusCode: test.status}
			var reference string
			transport := Transport(referenceRoundTripper(func(req *http.Request) (*http.Response, error) {
				reference = req.Header.Get(supportref.Header)
				return response, test.err
			}), "https://control.invalid")
			request, _ := http.NewRequest("GET", "https://control.invalid/PRIVATE", nil)
			got, err := transport.RoundTrip(request)
			if got == nil || got.StatusCode != response.StatusCode || got.Body != response.Body || !errors.Is(err, test.err) || got.Request == nil || got.Request.Header.Get(supportref.Header) != reference || response.Request != nil {
				t.Fatal("transport status/body/cause/owned-reference contract changed")
			}
			if test.err != nil && (!HTTPAttemptObserved(err) || err.Error() != "control request failed") {
				t.Fatal("recorded transport failure lost its safe marker")
			}
			if observed.Cause != test.cause || observed.Outcome != test.outcome || observed.SupportReference != reference || !supportref.Valid(reference) {
				t.Fatalf("fault=%+v", observed)
			}
		})
	}
}

func TestObservedAttemptEmitsOneFaultLogAndNoException(t *testing.T) {
	transport := &sentry.MockTransport{}
	r := newSignalReporter("https://public@example.invalid/1", "test", transport, true, true, 1)
	restore := Install(r)
	defer restore()
	client := Transport(referenceRoundTripper(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 503}, nil }), "https://control.invalid")
	request, _ := http.NewRequest("GET", "https://control.invalid/PRIVATE", nil)
	if _, err := client.RoundTrip(request); err != nil {
		t.Fatal(err)
	}
	r.Flush(context.Background())
	var logs, exceptions, counts, durations int
	for _, event := range transport.Events() {
		exceptions += len(event.Exception)
		for _, log := range event.Logs {
			if log.Attributes["operation"].AsString() == "control_request" {
				logs++
			}
		}
		for _, metric := range event.Metrics {
			if metric.Name == "paperboat.operation.count" {
				counts++
			}
			if metric.Name == "paperboat.operation.duration" {
				durations++
			}
		}
	}
	if logs != 1 || exceptions != 0 || counts != 1 || durations != 1 {
		t.Fatalf("logs=%d exceptions=%d counts=%d durations=%d", logs, exceptions, counts, durations)
	}
}

func TestFaultHTTPStatusIsPreservedAndUntrustedValuesAreDiscarded(t *testing.T) {
	transport := &sentry.MockTransport{}
	r := newSignalReporter("https://public@example.invalid/1", "test", transport, true, false, 0)
	var local Fault
	restore := InstallFaultObserver(func(_ context.Context, f Fault) { local = f })
	defer restore()
	r.CaptureFailure(context.Background(), "pb", "connect", "control_request", "control_request_failed", statusFault(503))
	r.Flush(context.Background())
	var logFound, exceptionFound bool
	for _, event := range transport.Events() {
		if len(event.Exception) > 0 && event.Tags["http_status"] == "503" {
			exceptionFound = true
		}
		for _, log := range event.Logs {
			if log.Attributes["http_status"].AsString() == "503" {
				logFound = true
			}
		}
	}
	if local.HTTPStatus != 503 || !logFound || !exceptionFound {
		t.Fatal("safe HTTP status missing from local or exported evidence")
	}
	for _, value := range []string{"200", "600", "0503", "503PRIVATE", "https://PRIVATE/503"} {
		event := &sentry.Event{Tags: map[string]string{"http_status": value}, Exception: []sentry.Exception{{Type: "internal_failure"}}}
		if sanitize(event, nil).Tags["http_status"] != "" {
			t.Fatal("untrusted event status retained")
		}
		attrs := cleanAttributes(map[string]attribute.Value{"http_status": attribute.StringValue(value)}, false)
		if _, retained := attrs["http_status"]; retained {
			t.Fatal("untrusted log status retained")
		}
	}
}

type rejectionWrapper struct{ cause error }

func (rejectionWrapper) Error() string         { panic("private response must not be formatted") }
func (e rejectionWrapper) Unwrap() error       { return e.cause }
func (rejectionWrapper) DiagnosticStatus() int { return 401 }

func TestFaultRejectionCannotHideOperationalBranches(t *testing.T) {
	deep := error(statusFault(401))
	for range 18 {
		deep = fmt.Errorf("PRIVATE: %w", deep)
	}
	oversized := make([]error, 18)
	for i := range oversized {
		oversized[i] = statusFault(401)
	}
	for _, test := range []struct {
		name           string
		err            error
		outcome, cause string
		errno          int
	}{
		{"nil cause status", rejectionWrapper{}, "rejected", "permission_denied", 0},
		{"transparent status", rejectionWrapper{observedHTTPFailure{cause: statusFault(401), observed: true}}, "rejected", "permission_denied", 0},
		{"ordinary canceled cleanup", errors.Join(statusFault(401), context.Canceled), "rejected", "permission_denied", 0},
		{"storage", errors.Join(statusFault(401), syscall.EIO), "failed", "system_error", int(syscall.EIO)},
		{"unknown", errors.Join(statusFault(401), errors.New("PRIVATE")), "failed", "internal", 0},
		{"deadline", errors.Join(statusFault(401), context.DeadlineExceeded), "failed", "deadline_exceeded", 0},
		{"cycle", errors.Join(statusFault(401), &cyclicFault{}), "failed", "internal", 0},
		{"typed nil", errors.Join(statusFault(401), (*cyclicFault)(nil)), "failed", "internal", 0},
		{"depth", errors.Join(statusFault(401), deep), "failed", "internal", 0},
		{"size", errors.Join(oversized...), "failed", "internal", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			fault := ProjectFault(t.Context(), "pb", "auth", "peer_authority", "peer_authority_failed", test.err)
			if fault.Outcome != test.outcome || fault.Cause != test.cause || fault.Errno != test.errno || fault.HTTPStatus != 401 {
				t.Fatalf("fault=%+v", fault)
			}
			if test.errno != 0 && fault.ErrorType != "Errno" {
				t.Fatal("error type does not identify the selected operational cause")
			}
		})
	}
}
