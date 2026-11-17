package diagnostics

import (
	"slices"

	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
	"net/http"
	"time"
)

type providerTransport struct {
	provider string
	next     http.RoundTripper
	now      func() time.Time
}

func DefaultProviderClient(provider string) *http.Client {
	//paperboat:allow-source-policy default-http owner=server-diagnostics reason=instrument-standard-transport-behind-bounded-client
	return &http.Client{Timeout: 30 * time.Second, Transport: InstrumentProviderTransport(provider, http.DefaultTransport)}
}

func InstrumentProviderTransport(provider string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		//paperboat:allow-source-policy default-http owner=server-diagnostics reason=instrument-standard-transport-fallback
		next = http.DefaultTransport
	}
	return providerTransport{provider: normalizeProvider(provider), next: next, now: time.Now}
}

func (t providerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	now := t.now
	if now == nil {
		now = time.Now
	}
	started := now()
	ctx := request.Context()
	finish := func(string) {}
	if observer, ok := ctx.Value(providerObserverKey{}).(ProviderObserver); ok {
		ctx, finish = observer(ctx, t.provider)
	}
	response, err := t.next.RoundTrip(request.Clone(ctx))
	outcome := "success"
	if err != nil || response == nil || response.StatusCode >= 500 {
		outcome = "failed"
	} else if response.StatusCode >= 400 {
		outcome = "rejected"
	}
	if errors.Is(request.Context().Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		outcome = "canceled"
	}
	finish(outcome)
	latency := max(0, now().Sub(started).Milliseconds())
	prefix := "provider_" + t.provider
	count(prefix+"_requests_total", 1)
	count(prefix+"_latency_ms_total", latency)
	raise(prefix+"_latency_ms_max", latency)
	if outcome == "failed" {
		count(prefix+"_errors_total", 1)
	}
	return response, err
}

func normalizeProvider(provider string) string {
	if slices.Contains(telemetry.PlatformProviders, provider) {
		return provider
	}
	return "unknown"
}

type ProviderObserver func(context.Context, string) (context.Context, func(string))
type providerObserverKey struct{}

// WithProviderObserver attaches metadata-only observation to this operation;
// outgoing provider headers and request content are never inspected or changed.
func WithProviderObserver(ctx context.Context, observer ProviderObserver) context.Context {
	return context.WithValue(ctx, providerObserverKey{}, observer)
}
