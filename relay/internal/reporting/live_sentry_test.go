package reporting

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestLiveSentryFailureRecovery(t *testing.T) {
	dsn, ref := os.Getenv("PB_LIVE_SENTRY_DSN"), os.Getenv("PB_LIVE_SENTRY_REF")
	if dsn == "" || ref == "" {
		t.Skip("set PB_LIVE_SENTRY_DSN and PB_LIVE_SENTRY_REF")
	}
	if !validReference(ref) {
		t.Fatal("PB_LIVE_SENTRY_REF must be pb-32lowerhex")
	}
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "true")
	t.Setenv("PAPERBOAT_SENTRY_DSN", dsn)
	t.Setenv("PAPERBOAT_SENTRY_RELEASE", "paperboat-relay:validation-20260919")
	t.Setenv("PAPERBOAT_SENTRY_ENVIRONMENT", "sentry-validation")
	t.Setenv("PAPERBOAT_SENTRY_REGION", "validation")
	t.Setenv("PAPERBOAT_SENTRY_INSTANCE", "slot-01")
	t.Setenv("PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE", "1")
	r, err := New("paperboat-relay")
	if err != nil {
		t.Fatal("live reporter configuration rejected")
	}
	defer r.Close()
	calls := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		calls++
		if q.Header.Get("Support-Reference") == "" {
			t.Fatal("missing control reference")
		}
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	for i, want := range []int{503, 200} {
		trace, requestRef, finish := r.ControlTrace(context.Background(), "dependency_health")
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/status", nil)
		req.Header.Set("Support-Reference", requestRef)
		if trace != "" {
			req.Header.Set("sentry-trace", trace)
		}
		resp, e := srv.Client().Do(req)
		if e != nil || resp.StatusCode != want {
			t.Fatal("control validation request failed")
		}
		resp.Body.Close()
		outcome, code := "failed", "unavailable"
		if i == 1 {
			outcome, code = "success", "recovered"
		}
		finish(outcome, code)
	}
	r.Observe(context.Background(), "relay_service", "failed", "unavailable", ref, time.Millisecond)
	r.Observe(context.Background(), "relay_service", "success", "recovered", ref, time.Millisecond)
	r.Capture(ref, "live_validation", 0)
	r.ExportDrops(context.Background())
	r.MetricSnapshots(context.Background(), []Snapshot{{Name: "paperboat_relay_sessions", Kind: "gauge", Value: 1}})
	t.Logf("live_sentry correlated_reference=%s control_references=per_request signals=error,log,trace,metric,drop_health", ref)
}
