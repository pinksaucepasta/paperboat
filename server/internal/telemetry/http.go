package telemetry

import (
	"context"
	"github.com/pinksaucepasta/paperboat/shared/health"
	sharedmetrics "github.com/pinksaucepasta/paperboat/shared/metrics"
	sharedtelemetry "github.com/pinksaucepasta/paperboat/shared/telemetry"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
)

// HTTPIdentity extracts safe request and correlation IDs already assigned by
// trusted middleware. It must never return headers, URLs, bodies, or tokens.
type HTTPIdentity func(context.Context) (requestID, correlationID string)

// HTTPObserver records bounded server request telemetry. The route family is
// caller-supplied from a finite allowlist so user-controlled paths and
// hostnames can never become metric labels.
type HTTPObserver struct {
	Metrics *sharedmetrics.Metrics
	// Routes, when set, receives the per-route-template request counter and duration. It is a registry of its
	// own: hundreds of route series must not share the series cap or the Sentry snapshot of Metrics.
	Routes   *sharedmetrics.Metrics
	Events   *sharedtelemetry.EventLog
	Identity HTTPIdentity
	Now      func() time.Time
}

func (o HTTPObserver) Wrap(routeFamily string, next http.Handler) http.Handler {
	return o.WrapFunc(func(*http.Request) string { return routeFamily }, next)
}

// WrapFunc classifies requests into a finite route family at request time.
// The classifier output is normalized before it can reach metrics or events.
func (o HTTPObserver) WrapFunc(routeFamily func(*http.Request) string, next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		family := "other"
		if routeFamily != nil {
			family = normalizeHTTPRouteFamily(routeFamily(r))
		}
		started := o.now()
		matched := &routeCapture{}
		captured := httpsnoop.CaptureMetrics(next, w, r.WithContext(context.WithValue(r.Context(), routeCaptureKey{}, matched)))
		statusClass := httpStatusClass(captured.Code)
		outcome := sharedtelemetry.OutcomeSuccess
		severity := sharedtelemetry.SeverityInfo
		code := "http_request_succeeded"
		message := "HTTP request completed."
		if captured.Code >= http.StatusBadRequest && captured.Code < http.StatusInternalServerError {
			outcome, severity, code, message = sharedtelemetry.OutcomeRejected, sharedtelemetry.SeverityWarn, "http_request_rejected", "HTTP request was rejected."
		} else if captured.Code >= http.StatusInternalServerError {
			outcome, severity, code, message = sharedtelemetry.OutcomeFailed, sharedtelemetry.SeverityError, "http_request_failed", "HTTP request failed."
		}
		if o.Metrics != nil {
			_ = o.Metrics.IncCounter(MetricHTTPRequests, sharedmetrics.Labels{
				"method": methodLabel(r.Method), "route_family": family, "status_class": statusClass,
			})
			_ = o.Metrics.ObserveHistogram(MetricHTTPDuration, sharedmetrics.Labels{"route_family": family, "status_class": statusClass}, captured.Duration.Seconds())
		}
		o.recordRoute(r.Method, matched, statusClass, captured.Duration)
		if o.Events == nil || o.Identity == nil {
			return
		}
		requestID, correlationID := o.Identity(r.Context())
		if requestID == "" || correlationID == "" {
			return
		}
		_, _ = o.Events.Record(sharedtelemetry.EventInput{
			At: started.Add(captured.Duration), Severity: severity, Component: health.DimensionService,
			Name: "http_request", Code: code, Outcome: outcome, Message: message,
			RequestID: requestID, CorrelationID: correlationID, Retry: health.RetryNone,
		})
	})
}

// UnmatchedRoute is the route label of a request no registered pattern matched.
const UnmatchedRoute = "unmatched"

type routeCaptureKey struct{}

// routeCapture carries the mux pattern a request matched from the router, which knows it, out to the observer,
// which wraps the router: the mux sets the pattern on its own copy of the request, not on the observer's.
type routeCapture struct{ pattern atomic.Pointer[string] }

// SetMatchedRoute records the mux pattern the request matched, for the route metrics of the observer that wraps it.
// pattern must come from the mux (http.ServeMux.Handler), never from the request.
func SetMatchedRoute(r *http.Request, pattern string) {
	if capture, ok := r.Context().Value(routeCaptureKey{}).(*routeCapture); ok {
		capture.pattern.Store(&pattern)
	}
}

// routeTemplate turns a mux pattern ("GET /v1/machines/{id}/events") into its path template.
func routeTemplate(pattern string) string {
	if _, path, found := strings.Cut(pattern, " "); found {
		pattern = path
	}
	if pattern == "" {
		return UnmatchedRoute
	}
	return pattern
}

func (o HTTPObserver) recordRoute(method string, matched *routeCapture, statusClass string, duration time.Duration) {
	if o.Routes == nil {
		return
	}
	route := UnmatchedRoute
	if pattern := matched.pattern.Load(); pattern != nil {
		route = routeTemplate(*pattern)
	}
	if o.Routes.AllowLabelValue(MetricHTTPRouteRequests, "route", route) != nil || o.Routes.AllowLabelValue(MetricHTTPRouteDuration, "route", route) != nil {
		route = UnmatchedRoute
	}
	_ = o.Routes.IncCounter(MetricHTTPRouteRequests, sharedmetrics.Labels{"method": methodLabel(method), "route": route, "status_class": statusClass})
	_ = o.Routes.ObserveHistogram(MetricHTTPRouteDuration, sharedmetrics.Labels{"route": route, "status_class": statusClass}, duration.Seconds())
}

func (o HTTPObserver) now() time.Time {
	if o.Now == nil {
		return time.Now().UTC()
	}
	return o.Now().UTC()
}

func normalizeHTTPRouteFamily(value string) string {
	switch value {
	case "health", "public_api", "edge_control", "release", "internal":
		return value
	default:
		return "other"
	}
}

func methodLabel(method string) string {
	switch strings.ToUpper(method) {
	case http.MethodGet:
		return "get"
	case http.MethodPost:
		return "post"
	case http.MethodPut:
		return "put"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		return "delete"
	default:
		return "other"
	}
}

func httpStatusClass(status int) string {
	if status < 100 {
		status = http.StatusOK
	}
	switch status / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	default:
		return "5xx"
	}
}
