package httpapi

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/ratelimit"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/google/uuid"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

func safeLogPath(path string) string {
	if strings.HasPrefix(path, "/v1/auth/device/requests/") {
		suffix := strings.TrimPrefix(path, "/v1/auth/device/requests/")
		switch {
		case strings.HasSuffix(suffix, "/approve"):
			return "/v1/auth/device/requests/{user_code}/approve"
		case strings.HasSuffix(suffix, "/deny"):
			return "/v1/auth/device/requests/{user_code}/deny"
		default:
			return "/v1/auth/device/requests/{user_code}"
		}
	}
	return path
}

func recoverer(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				writePanicError(w, r, logger, value)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func timeout(duration time.Duration, logger *slog.Logger, next http.Handler) http.Handler {
	if duration <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStreamingRoute(r) {
			next.ServeHTTP(w, r)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), duration)
		defer cancel()

		tw := newTimeoutResponseWriter(w)
		results := make(chan handlerResult, 1)
		go func() {
			defer func() {
				if value := recover(); value != nil {
					results <- handlerResult{panicValue: value, panicStack: sentry.NewStacktrace()}
					return
				}
				results <- handlerResult{}
			}()
			next.ServeHTTP(tw, r.WithContext(ctx))
		}()

		var result handlerResult
		select {
		case result = <-results:
		case <-ctx.Done():
		}
		// A handler can return because its deadline expired. Completion and
		// cancellation may both be ready; that must not produce an empty 200.
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				tw.markTimedOut()
				return
			}
			if tw.markTimedOut() {
				writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "Request timed out.", err)
			}
			return
		}
		if result.panicValue != nil {
			writePanicError(w, r, logger, result.panicValue, result.panicStack)
		}

	})
}

func isReleaseDownload(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/selfhost/") || r.URL.Path == "/install" || strings.HasPrefix(r.URL.Path, "/tuf/") || strings.HasPrefix(r.URL.Path, "/helper-releases/tuf/")
}

// streamingRoutePath matches the GET routes that hold a response or socket open by design: the preview and
// tunnel event streams and the edge's browser-terminal control socket. Each one bounds its own lifetime.
var streamingRoutePath = regexp.MustCompile(`^/v1/(previews/[^/]+/events|tunnels/[^/]+/events|edge/browser-terminal/control)$`)

// isStreamingRoute reports whether r is on the streaming allow-list, the only routes that skip the request
// timeout. Request headers never decide it: an ordinary route asked for text/event-stream or an upgrade is
// still a request that must finish within the request timeout.
func isStreamingRoute(r *http.Request) bool {
	if r.Method == http.MethodGet && streamingRoutePath.MatchString(r.URL.Path) {
		// An event route streams only when asked for its event-stream representation; its JSON
		// representation is an ordinary request and keeps the request timeout.
		return !strings.HasSuffix(r.URL.Path, "/events") || acceptsEventStream(r)
	}
	return isMachineEventsWait(r.URL.Path) || isReleaseDownload(r)
}

// acceptsEventStream reports whether the client asked for a text/event-stream representation. It picks the
// representation of an event route only; it never changes the timeout.
func acceptsEventStream(r *http.Request) bool {
	for _, accept := range r.Header.Values("Accept") {
		for _, part := range strings.Split(accept, ",") {
			mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
			if mediaType == "text/event-stream" {
				return true
			}
		}
	}
	return false
}

func bodyLimit(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestLimit := limit
		if strings.HasPrefix(r.URL.Path, "/v1/environment-") || strings.Contains(r.URL.Path, "/environment-manifests/") {
			requestLimit = 3 << 20
		} else if strings.HasPrefix(r.URL.Path, "/v1/environment/") {
			requestLimit = 2 << 20
		}
		if r.Body != nil && requestLimit > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, requestLimit)
		}
		next.ServeHTTP(w, r)
	})
}

func cors(allowedOrigins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(allowedOrigins, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Expose-Headers", "Support-Reference, Request-Id, Correlation-Id, Paperboat-Commit-Position")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Support-Reference, Correlation-Id, Idempotency-Key, If-Match, If-None-Match, X-CSRF-Token, Operator-Target, Operator-Reason, Paperboat-Commit-Position")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func writePanicError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, value any, stacks ...*sentry.Stacktrace) {
	stack := sentry.NewStacktrace()
	if len(stacks) > 0 {
		stack = stacks[0]
	}
	captureSupportFailure(r, 500, "panic", stack, supportreport.ErrPanic)
	// The panic is already captured with its initiating stack; do not report a
	// second generic internal_error from the response writer.
	writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: APIError{Code: "internal_error", Message: "Internal server error.", RequestID: requestIDFromContext(r.Context()), SupportReference: supportReference(r.Context()), Details: map[string]any{}}})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	// ctx is the request's: a server error written after the client hung up is that, not an outage.
	ctx context.Context
}

func (r *statusRecorder) WriteHeader(status int) {
	if status >= 500 && r.ctx != nil && errors.Is(r.ctx.Err(), context.Canceled) {
		status = statusClientClosedRequest
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) FlushError() error {
	if flusher, ok := r.ResponseWriter.(interface{ FlushError() error }); ok {
		return flusher.FlushError()
	}
	r.Flush()
	return nil
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

type handlerResult struct {
	panicStack *sentry.Stacktrace
	panicValue any
}

type timeoutResponseWriter struct {
	dst      http.ResponseWriter
	header   http.Header
	mu       sync.Mutex
	started  bool
	timedOut bool
}

func newTimeoutResponseWriter(dst http.ResponseWriter) *timeoutResponseWriter {
	return &timeoutResponseWriter{
		dst:    dst,
		header: make(http.Header),
	}
}

func (w *timeoutResponseWriter) Header() http.Header {
	return w.header
}

func (w *timeoutResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timedOut || w.started {
		return
	}
	w.started = true
	copyHeader(w.dst.Header(), w.header)
	w.dst.WriteHeader(status)
}

func (w *timeoutResponseWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timedOut {
		return 0, context.DeadlineExceeded
	}
	if !w.started {
		w.started = true
		copyHeader(w.dst.Header(), w.header)
	}
	return w.dst.Write(b)
}

func (w *timeoutResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timedOut {
		return
	}
	w.started = true
	copyHeader(w.dst.Header(), w.header)
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *timeoutResponseWriter) markTimedOut() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		w.timedOut = true
		return false
	}
	w.timedOut = true
	return true
}

func (w *timeoutResponseWriter) Unwrap() http.ResponseWriter {
	return w.dst
}

var errFlusherUnsupported = errors.New("response writer does not support flushing")

func (w *timeoutResponseWriter) FlushError() error {
	if _, ok := w.dst.(http.Flusher); !ok {
		return errFlusherUnsupported
	}
	w.Flush()
	return nil
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func requestIDFromContext(ctx context.Context) string {
	return serverdiagnostics.RequestID(ctx)
}

func newRequestID() string { return "request_" + uuid.NewString() }

// markResendable tells proof verification that HTTP may send this request again (it carries an
// Idempotency-Key), so a repeated proof nonce is not a replay (F21-13).
func markResendable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "" {
			r = r.WithContext(db.WithResendableRequest(r.Context()))
		}
		next.ServeHTTP(w, r)
	})
}

// signInRoutes present a credential before anyone is signed in. CLI device-token polling is not
// here: it is bounded by its device code and the database limit in auth.DeviceService.
var signInRoutes = map[string]bool{
	"GET /v1/auth/workos/state":        true,
	"POST /v1/auth/workos/callback":    true,
	"POST /v1/auth/device/authorize":   true,
	"POST /v1/auth/device/cancel":      true,
	"POST /v1/auth/api-token-sessions": true,
	"POST /v1/operator/login":          true,
	"POST /v1/operator/email":          true,
	"POST /v1/operator/setup":          true,
}

// failureLimitedRoutes exchange a secret (enrollment token, pairing verifier, join code) before
// anyone is signed in. Only rejected attempts use up the network's budget, so a fleet behind one
// NAT enrolling or renewing is not throttled; guessing is (OWASP Authentication Cheat Sheet,
// "Protect Against Automated Attacks"; same Check-then-Allow-on-failure shape as apiTokenAuth).
var failureLimitedRoutes = map[string]bool{
	"POST /v1/machines/pairings":              true,
	"POST /v1/machines/pairings/reset":        true,
	"POST /v1/machines/pairings/installation": true,
	"POST /v1/helper-enrollments":             true,
	"POST /v1/helper-identity-renewals":       true,
}

// failureLimited covers the routes above plus the guest terminal join-code and ticket routes,
// whose path carries a session id: guesses must not spread across sessions.
func failureLimited(r *http.Request) bool {
	if failureLimitedRoutes[r.Method+" "+r.URL.Path] {
		return true
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/terminal-sessions/")
	return ok && r.Method == http.MethodPost && (strings.HasSuffix(rest, "/code-join") || strings.HasSuffix(rest, "/guest-browser-ticket"))
}

// signInLimit limits sign-in requests per client network in memory, before any database work
// (PLAN F21-14, Lane X #15), and failed secret exchanges the same way.
func signInLimit(limiter *ratelimit.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if signInRoutes[r.Method+" "+r.URL.Path] {
			if ok, wait := limiter.Allow("sign-in:" + resolvedRequestNetwork(r)); !ok {
				writeRateLimited(w, r, wait)
				return
			}
		} else if failureLimited(r) {
			key := "exchange-failure:" + resolvedRequestNetwork(r)
			if ok, wait := limiter.Check(key); !ok {
				writeRateLimited(w, r, wait)
				return
			}
			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(recorder, r)
			if recorder.status >= 400 && recorder.status < 500 {
				limiter.Allow(key)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeRateLimited(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
	writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many attempts from your network. Wait a moment and try again.")
}
