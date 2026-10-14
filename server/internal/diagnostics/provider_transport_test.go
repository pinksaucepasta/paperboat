package diagnostics

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestProviderTransportRecordsBoundedLatencyAndErrors(t *testing.T) {
	providerMetrics.Lock()
	providerMetrics.values = make(map[string]providerMetric)
	providerMetrics.Unlock()

	clock := []time.Time{time.Unix(100, 0), time.Unix(100, 2*int64(time.Millisecond))}
	clockIndex := 0
	transport := providerTransport{provider: "stripe", next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable}, nil
	}), now: func() time.Time {
		value := clock[min(clockIndex, len(clock)-1)]
		clockIndex++
		return value
	}}
	request, err := http.NewRequest(http.MethodGet, "https://stripe.example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatal(err)
	}
	metrics := MetricsSnapshot()
	if metrics["provider_stripe_requests_total"] != 1 || metrics["provider_stripe_errors_total"] != 1 {
		t.Fatalf("provider metrics = %#v", metrics)
	}
	if metrics["provider_stripe_latency_ms_total"] < 1 || metrics["provider_stripe_latency_ms_max"] < 1 {
		t.Fatalf("provider latency metrics = %#v", metrics)
	}
}

func TestProviderTransportCountsNetworkFailureAndBoundsUnknownProvider(t *testing.T) {
	providerMetrics.Lock()
	providerMetrics.values = make(map[string]providerMetric)
	providerMetrics.Unlock()

	transport := InstrumentProviderTransport("user-controlled-value", roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	}))
	request, err := http.NewRequest(http.MethodGet, "https://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("expected transport error")
	}
	metrics := MetricsSnapshot()
	if metrics["provider_unknown_requests_total"] != 1 || metrics["provider_unknown_errors_total"] != 1 {
		t.Fatalf("unknown provider metrics = %#v", metrics)
	}
}

func TestDefaultProviderClientHasFiniteTimeoutAndInstrumentation(t *testing.T) {
	client := DefaultProviderClient("github")
	if client.Timeout <= 0 {
		t.Fatalf("timeout = %s, want finite timeout", client.Timeout)
	}
	transport, ok := client.Transport.(providerTransport)
	if !ok || transport.provider != "github" || transport.next == nil {
		t.Fatalf("transport = %#v, want instrumented github transport", client.Transport)
	}
}

func TestProviderTransportKeepsDeadlinesFailedAndCancellationExpected(t *testing.T) {
	cases := []struct {
		name     string
		context  func() (context.Context, context.CancelFunc)
		cause    error
		outcome  string
		failures int64
	}{
		{
			name: "deadline",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			cause: context.DeadlineExceeded, outcome: "failed", failures: 1,
		},
		{
			name: "canceled",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			cause: context.Canceled, outcome: "canceled", failures: 0,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			providerMetrics.Lock()
			providerMetrics.values = make(map[string]providerMetric)
			providerMetrics.Unlock()

			ctx, cancel := test.context()
			defer cancel()
			observedOutcome := ""
			ctx = WithProviderObserver(ctx, func(ctx context.Context, _ string) (context.Context, func(string)) {
				return ctx, func(outcome string) { observedOutcome = outcome }
			})
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://provider.example.test", nil)
			if err != nil {
				t.Fatal(err)
			}
			transport := InstrumentProviderTransport("workos", roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, test.cause
			}))
			if _, err := transport.RoundTrip(request); !errors.Is(err, test.cause) {
				t.Fatalf("round trip error=%v, want %v", err, test.cause)
			}
			if observedOutcome != test.outcome {
				t.Fatalf("observed outcome=%q, want %q", observedOutcome, test.outcome)
			}
			if got := MetricsSnapshot()["provider_workos_errors_total"]; got != test.failures {
				t.Fatalf("provider error count=%d, want %d", got, test.failures)
			}
		})
	}
}
