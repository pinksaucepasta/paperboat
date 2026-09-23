package errorreport_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostd"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/observability"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/peerattempt"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestAPIClientCarriesInvocationTraceAndSupportReference(t *testing.T) {
	const reference = "pb-0123456789abcdef0123456789abcdef"
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
	ctx, end := r.Start(supportref.WithContext(context.Background(), "pb-0123456789abcdef0123456789abcdef"), "paperboat-daemon", "daemon")
	_ = ctx
	registry, err := observability.NewRegistry(observability.DefaultDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	events, err := observability.NewEventLog(4)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	for _, outcome := range []observability.EventOutcome{observability.OutcomeFailed, observability.OutcomeSuccess} {
		_, err = events.Record(observability.EventInput{At: time.Now(), Severity: observability.SeverityInfo, Component: observability.DimensionRoute, Name: "route_transition", Code: "route_state", Outcome: outcome, Message: "PRIVATE-MARKER", CorrelationID: "corr_test", Retry: observability.RetryNone})
		if err != nil {
			t.Fatal(err)
		}
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
	for _, event := range transport.Events() {
		for _, log := range event.Logs {
			if log.Body != "paperboat.operation" {
				t.Fatal("raw message exported")
			}
			if log.Attributes["operation"].AsString() == "route_transition" {
				failed = failed || log.Attributes["outcome"].AsString() == "failed"
				recovered = recovered || log.Attributes["outcome"].AsString() == "success"
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
	if !failed || !recovered || !metric {
		t.Fatalf("failed=%v recovered=%v metric=%v", failed, recovered, metric)
	}
}

type machineCredential struct{}

func (machineCredential) Token(context.Context) (string, error) { return "PRIVATE-CREDENTIAL", nil }
func (machineCredential) Proof(context.Context, string, string, string, []byte) ([]byte, error) {
	return []byte("PRIVATE-PROOF"), nil
}
func TestIndependentMachineControlClientPropagatesTrace(t *testing.T) {
	transport := &sentry.MockTransport{}
	r := errorreport.NewTestReporter("https://public@example.invalid/1", "test", transport, true, true, 1)
	defer r.Flush(context.Background())
	restore := errorreport.Install(r)
	defer restore()
	ctx, end := r.Start(supportref.WithContext(context.Background(), "pb-0123456789abcdef0123456789abcdef"), "paperboat-daemon", "peer_identity")
	defer end("success")
	parent := sentry.SpanFromContext(ctx)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.Header.Get("sentry-trace"), parent.TraceID.String()+"-") || req.Header.Get(supportref.Header) == "" {
			t.Error("machine control correlation missing")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := peerattempt.New(peerattempt.Config{ControlURL: server.URL, StateRoot: t.TempDir(), Transport: server.Client().Transport}, machineCredential{})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Reject(ctx, api.PeerAttemptDescriptor{IntentID: "intent_01", AttemptGeneration: 1}); err != nil {
		t.Fatal(err)
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
