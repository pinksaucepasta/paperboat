package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/google/uuid"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
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
		if isStreamingRequest(r) || isReleaseDownload(r) || isMachineEventsWait(r.URL.Path) {
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

func isStreamingRequest(r *http.Request) bool {
	if r != nil && strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		for _, value := range r.Header.Values("Connection") {
			for _, token := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
					return true
				}
			}
		}
	}
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
			w.Header().Set("Access-Control-Expose-Headers", "Support-Reference, Request-Id, Correlation-Id")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Support-Reference, Correlation-Id, Idempotency-Key, If-Match, If-None-Match, X-CSRF-Token, Operator-Target, Operator-Reason")
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
}

func (r *statusRecorder) WriteHeader(status int) {
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
