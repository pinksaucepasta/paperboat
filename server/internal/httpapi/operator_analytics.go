package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/pinksaucepasta/paperboat/server/internal/posthogquery"
)

// registerOperatorAnalytics serves the product-analytics read proxy. All routes sit behind the operator guard, so
// every read is audited; the PostHog key never leaves the server and callers pick only an app and a day range.
func registerOperatorAnalytics(mux *http.ServeMux, opts Options) {
	mux.Handle("GET /v1/admin/analytics/status", operatorRoute(opts, analyticsStatus(opts.OperatorAnalytics)))
	mux.Handle("GET /v1/admin/analytics/site", operatorRoute(opts, analyticsRead(opts.OperatorAnalytics, siteRead)))
	mux.Handle("GET /v1/admin/analytics/console", operatorRoute(opts, analyticsRead(opts.OperatorAnalytics, consoleRead)))
}

func siteRead(ctx context.Context, s *posthogquery.Service, days int) (any, error) {
	return s.Site(ctx, days)
}

func consoleRead(ctx context.Context, s *posthogquery.Service, days int) (any, error) {
	return s.Console(ctx, days)
}

func analyticsStatus(service *posthogquery.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, SuccessResponse{Data: service.Status()})
	})
}

func analyticsRead(service *posthogquery.Service, read func(context.Context, *posthogquery.Service, int) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		days := 0
		if raw := r.URL.Query().Get("days"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				writeError(w, r, 400, "invalid_request", "Use days=7, 30 or 90.")
				return
			}
			days = n
		}
		out, err := read(r.Context(), service, days)
		switch {
		case err == nil:
			writeJSON(w, 200, SuccessResponse{Data: out})
		case errors.Is(err, posthogquery.ErrNotConfigured):
			writeError(w, r, 503, "analytics_not_configured", "Product analytics is not configured on this server.")
		case errors.Is(err, posthogquery.ErrInvalid):
			writeError(w, r, 400, "invalid_request", "Use days=7, 30 or 90.")
		default:
			writeError(w, r, 503, "analytics_unavailable", "The analytics provider did not answer. Try again in a minute.", err)
		}
	})
}
