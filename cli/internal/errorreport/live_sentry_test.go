package errorreport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestLiveSentryNativeFailureRecovery(t *testing.T) {
	dsn, reference := os.Getenv("PB_LIVE_SENTRY_DSN"), os.Getenv("PB_LIVE_SENTRY_REF")
	if dsn == "" || reference == "" {
		t.Skip("set PB_LIVE_SENTRY_DSN and PB_LIVE_SENTRY_REF")
	}
	if !supportref.Valid(reference) {
		t.Fatal("PB_LIVE_SENTRY_REF must be pb-32lowerhex")
	}
	for _, component := range []string{"paperboat-cli", "paperboat-daemon"} {
		t.Run(component, func(t *testing.T) {
			t.Setenv("PB_SENTRY_ENABLED", "true")
			t.Setenv("PB_SENTRY_DSN", dsn)
			t.Setenv("PB_SENTRY_RELEASE", component+":validation-20260919")
			t.Setenv("PB_SENTRY_ENVIRONMENT", "sentry-validation")
			t.Setenv("PB_SENTRY_LOGS_ENABLED", "true")
			t.Setenv("PB_SENTRY_METRICS_ENABLED", "true")
			t.Setenv("PB_SENTRY_TRACES_SAMPLE_RATE", "1")
			reporter := FromEnvironment()
			if !reporter.Enabled() {
				t.Fatal("live reporter configuration rejected")
			}
			restore := Install(reporter)
			defer restore()
			defer reporter.Flush(context.Background())
			ctx := supportref.WithContext(context.Background(), reference)
			ctx, complete := reporter.Start(ctx, component, "control_request")
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				calls++
				if request.Header.Get(supportref.Header) != reference {
					t.Fatal("control reference changed")
				}
				if calls == 1 {
					w.WriteHeader(503)
					return
				}
				w.WriteHeader(200)
			}))
			defer server.Close()
			transport := TransportOperation(server.Client().Transport, server.URL, "control_request")
			for index, want := range []int{503, 200} {
				request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/status", nil)
				response, err := transport.RoundTrip(request)
				if err != nil || response.StatusCode != want {
					t.Fatal("control validation request failed")
				}
				response.Body.Close()
				outcome := "failed"
				if index == 1 {
					outcome = "success"
				}
				reporter.Observe(ctx, component, "control_request", outcome, time.Millisecond)
			}
			complete("success")
			reporter.Capture(ctx, component, "unexpected_failure")
			reporter.RegisterMetrics(func() []MetricSample {
				return []MetricSample{{Name: "paperboat_reporting_signal_enabled", Value: 1, Labels: map[string]string{"signal": "metrics"}}, {Name: "paperboat_reporting_errors_dropped_total_snapshot", Value: 0}}
			}, []MetricDescriptor{{Name: "paperboat_reporting_signal_enabled", Labels: map[string]map[string]bool{"signal": {"metrics": true}}}, {Name: "paperboat_reporting_errors_dropped_total_snapshot"}})
			reporter.sampleMetrics()
			t.Logf("live_sentry reference=%s component=%s signals=error,log,trace,metric,drop_health", reference, component)
		})
	}
}
