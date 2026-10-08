package errorreport_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostd"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/observability"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestAPIClientCarriesInvocationTraceAndSupportReference(t *testing.T) {
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	r := errorreport.NewTestReporter("https://public@example.invalid/1", "test", &sentry.MockTransport{}, true, true, 1)
	defer r.Flush(context.Background())
	restore := errorreport.Install(r)
	defer restore()
	ctx, end := r.Start(supportref.WithContext(context.Background(), reference), "paperboat-cli", "status")
	defer end("rejected")
	parent := sentry.SpanFromContext(ctx)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get(supportref.Header) != reference {
			t.Error("support reference lost")
		}
		if !strings.HasPrefix(req.Header.Get("sentry-trace"), parent.TraceID.String()+"-") {
			t.Error("invocation trace lost")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"denied"}}`))
	}))
	defer server.Close()
	_, err := api.New(server.URL, config.Credential{}, server.Client()).Me(ctx)
	if err == nil {
		t.Fatal("API denial swallowed")
	}
}

func TestRuntimeLifecycleFailureRecoveryAndRegistryExport(t *testing.T) {
	transport := &sentry.MockTransport{}
	r := errorreport.NewTestReporter("https://public@example.invalid/1", "test", transport, true, true, 1)
	defer r.Flush(context.Background())
	restore := errorreport.Install(r)
	defer restore()
	ctx, end := r.Start(supportref.WithContext(context.Background(), "support_01234567-89ab-4def-8123-456789abcdef"), "paperboat-daemon", "daemon")
	local := diagnostics.NewMemoryRecorder()
	ctx = diagnostics.WithRecorder(ctx, local)
	var faults []errorreport.Fault
	restoreFaults := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) {
		faults = append(faults, f)
		if err := local.RecordFault(f); err != nil {
			t.Fatal(err)
		}
	})
	defer restoreFaults()
	registry, err := observability.NewRegistry(observability.DefaultDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	events, err := observability.NewEventLog(4)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	// Exercise the actual component lifecycle owner, with its optional memory
	// event log attached. The event log itself must not export a second fault.
	for _, fail := range []bool{true, false} {
		runtime, err := hostruntime.NewRuntime(hostruntime.Config{Version: "test", EventLog: events, Components: []hostruntime.Component{
			{Capability: "worker_lifecycle", Required: false, Service: &lifecycleService{fail: fail}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(faults) != 1 || faults[0].Stage != "component_start" || faults[0].Code != "start_failed" || faults[0].Cause != "internal" {
		t.Fatalf("faults=%#v", faults)
	}
	if got := local.Recent(); len(got) != 3 || got[0].Code != "start_failed" || got[1].Code != "ready" || got[2].Code != "stopped" {
		t.Fatalf("local lifecycle=%#v", got)
	}
	if err = registry.Record("paperboat_runtime_connector_retries_total", 2, map[string]string{"transport": "quic", "result": "connected"}); err != nil {
		t.Fatal(err)
	}
	if err = events.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	end("success")
	r.Flush(context.Background())
	var failed, recovered, metric bool
	exceptions := 0
	for _, event := range transport.Events() {
		if len(event.Exception) > 0 {
			exceptions++
			if event.Tags["stage"] != "component_start" || event.Tags["cause"] != "internal" || event.Tags["support_reference"] != supportref.FromContext(ctx) {
				t.Fatal("typed failure lost stage, cause or correlation")
			}
		}
		encoded, _ := json.Marshal(event)
		if strings.Contains(string(encoded), "PRIVATE-MARKER") {
			t.Fatal("private component failure exported")
		}
		for _, log := range event.Logs {
			if log.Body != "paperboat.operation" {
				t.Fatal("raw message exported")
			}
			if log.Attributes["operation"].AsString() == "component_start" {
				failed = failed || log.Attributes["outcome"].AsString() == "failed"
				recovered = recovered || log.Attributes["outcome"].AsString() == "success" && log.Attributes["code"].AsString() == "ready"
			}
		}
		for _, m := range event.Metrics {
			if m.Name == "paperboat_runtime_connector_retries_total_snapshot" {
				metric = true
				if m.Attributes["dimension.transport"].AsString() != "quic" {
					t.Fatal("fixed dimension missing")
				}
			}
		}
	}
	if !failed || !recovered || !metric || exceptions != 1 {
		t.Fatalf("failed=%v recovered=%v metric=%v", failed, recovered, metric)
	}
}

type lifecycleService struct{ fail bool }

func (s *lifecycleService) Start(context.Context) error {
	if s.fail {
		return errors.New("PRIVATE-MARKER")
	}
	return nil
}
func (*lifecycleService) Shutdown(context.Context) error { return nil }
func TestProductionStableLifecycleExportsWithoutEventLog(t *testing.T) {
	transport := &sentry.MockTransport{}
	r := errorreport.NewTestReporter("https://public@example.invalid/1", "test", transport, true, false, 0)
	defer r.Flush(context.Background())
	restore := errorreport.Install(r)
	defer restore()
	service := &lifecycleService{fail: true}
	daemon, err := hostd.New(hostd.Config{Workloads: hostd.Workloads{Transfers: &filetransfer.Service{}}, Components: []hostd.Component{{Name: "storage", Required: true, Service: service}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = daemon.Start(context.Background()); err == nil {
		t.Fatal("startup failure swallowed")
	}
	service.fail = false
	if err = daemon.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = daemon.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Flush(context.Background())
	stages := map[string]bool{}
	for _, e := range transport.Events() {
		for _, l := range e.Logs {
			if l.Attributes["service_component"].AsString() != "storage" {
				continue
			}
			stages[l.Attributes["operation"].AsString()+":"+l.Attributes["code"].AsString()] = true
			b, _ := json.Marshal(l)
			if strings.Contains(string(b), "PRIVATE-MARKER") {
				t.Fatal("raw failure exported")
			}
		}
	}
	for _, stage := range []string{"component_start:start_failed", "component_rollback:stopped", "component_start:ready", "component_shutdown:stopped"} {
		if !stages[stage] {
			t.Errorf("missing stable lifecycle %s", stage)
		}
	}
}

func TestAPIStatusFailurePreservesOwnedHTTPAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"PRIVATE token must not enter diagnostics"}}`))
	}))
	defer server.Close()
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	ctx := supportref.WithContext(t.Context(), supportref.New())
	_, err := api.New(server.URL, config.Credential{}, server.Client()).Me(ctx)
	var response *api.APIError
	if !errors.As(err, &response) || response.Status != 503 || !errorreport.HTTPAttemptObserved(err) || len(faults) != 1 || faults[0].HTTPStatus != 503 {
		t.Fatalf("API response ownership lost: status=%v faults=%+v", response, faults)
	}
	if strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("private server contents exposed")
	}
	if errorreport.HTTPAttemptObserved(errors.Join(err, errors.New("independent failure"))) {
		t.Fatal("independent failure incorrectly suppressed")
	}
}
