// Package errorreport provides Paperboat's deliberately narrow Sentry boundary.
// Callers supply only a stable classification and support reference; raw errors,
// commands, request data, URLs, logs, breadcrumbs, and local values are never accepted.
package errorreport

import (
	"context"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

const (
	queueCapacity    = 8
	maximumPerMinute = 3
	flushLimit       = 2 * time.Second
)

type Reporter struct {
	ambientReference         atomic.Pointer[string]
	snapshotDropped          atomic.Uint64
	snapshotCursor           int
	component                atomic.Pointer[string]
	sources                  []func() []MetricSample
	metricSchema             map[string]map[string]map[string]bool
	samplerStop, samplerDone chan struct{}
	flushOnce                sync.Once
	client                   *sentry.Client
	mu                       sync.Mutex
	window                   time.Time
	count                    int
	logs, metrics            bool
	rate                     float64
	limits                   [3]signalLimit
	closed                   atomic.Bool
}

func FromEnvironment() *Reporter {
	enabled := buildinfo.DefaultSentryDSN != ""
	switch os.Getenv("PB_SENTRY_ENABLED") {
	case "":
	case "true":
		enabled = true
	case "false":
		enabled = false
	default:
		return &Reporter{}
	}
	if !enabled {
		return &Reporter{}
	}
	logs, logsOK := signalFlag("PB_SENTRY_LOGS_ENABLED", true)
	metrics, metricsOK := signalFlag("PB_SENTRY_METRICS_ENABLED", true)
	if !logsOK || !metricsOK {
		return &Reporter{}
	}
	dsn, release := strings.TrimSpace(os.Getenv("PB_SENTRY_DSN")), strings.TrimSpace(os.Getenv("PB_SENTRY_RELEASE"))
	if dsn == "" {
		dsn = buildinfo.DefaultSentryDSN
	}
	if release == "" {
		release = buildinfo.DefaultSentryRelease
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || !safeRelease(release) {
		return &Reporter{}
	}
	transport := sentry.NewHTTPTransport()
	transport.BufferSize = queueCapacity
	transport.Timeout = flushLimit
	rate := 0.1
	if raw := os.Getenv("PB_SENTRY_TRACES_SAMPLE_RATE"); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value < 0 || value > 1 || math.IsNaN(value) {
			return &Reporter{}
		}
		rate = value
	}
	return newSignalReporter(dsn, release, transport, logs, metrics, rate)
}

func newReporter(dsn, release string, transport sentry.Transport) *Reporter {
	return newSignalReporter(dsn, release, transport, false, false, 0)
}

func newSignalReporter(dsn, release string, transport sentry.Transport, logs, metrics bool, rate float64) *Reporter {
	r := &Reporter{logs: logs, metrics: metrics, rate: rate, metricSchema: make(map[string]map[string]map[string]bool)}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn: dsn, Release: release, Environment: environment(), Transport: transport,
		AttachStacktrace: false, EnableTracing: rate > 0, TracesSampleRate: rate, MaxSpans: 64, BeforeSendLog: sanitizeLog, BeforeSendMetric: r.sanitizeMetric, BeforeSendTransaction: sanitizeTransaction, MaxBreadcrumbs: 0,
		SendDefaultPII: false, BeforeSend: sanitize,
		Integrations: func([]sentry.Integration) []sentry.Integration { return nil },
	})
	if err != nil {
		return &Reporter{}
	}
	r.client = client
	if metrics {
		r.samplerStop = make(chan struct{})
		r.samplerDone = make(chan struct{})
		go r.runMetrics()
	}
	return r
}

func (r *Reporter) Enabled() bool { return r != nil && r.client != nil && !r.closed.Load() }

func (r *Reporter) Capture(ctx context.Context, component, classification string) {
	if !r.Enabled() || !safeComponent(component) || !safeClassification(classification) || !r.admit(time.Now()) {
		return
	}
	ctx = r.context(ctx)
	event := sentry.NewEvent()
	event.Level = sentry.LevelError
	event.Exception = []sentry.Exception{{Type: classification, Stacktrace: safeStack(2)}}
	event.Tags = map[string]string{"component": Component(component)}
	if value, ok := ctx.Value(lifecycleComponentKey{}).(string); ok {
		event.Tags["service_component"] = serviceComponent(value)
	}
	if span := sentry.SpanFromContext(ctx); span != nil {
		event.Contexts = map[string]sentry.Context{"trace": {"trace_id": span.TraceID, "span_id": span.SpanID}}
	}
	if reference := supportref.FromContext(ctx); reference != "" {
		event.Tags["support_reference"] = reference
	}
	r.client.CaptureEvent(event, nil, nil)
}

func (r *Reporter) Flush(ctx context.Context) {
	if r == nil || r.client == nil {
		return
	}
	r.flushOnce.Do(func() {
		if r.samplerStop != nil {
			close(r.samplerStop)
			<-r.samplerDone
		}
		r.closed.Store(true)
		flushCtx, cancel := context.WithTimeout(ctx, flushLimit)
		defer cancel()
		_ = r.client.FlushWithContext(flushCtx)
		r.client.Close()
	})
}

func safeRelease(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._+-:", c)) {
			return false
		}
	}
	return true
}

func (r *Reporter) admit(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.window.IsZero() || now.Sub(r.window) >= time.Minute {
		r.window, r.count = now, 0
	}
	if r.count >= maximumPerMinute {
		return false
	}
	r.count++
	return true
}

func safeClassification(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character != '_' && (character < 'a' || character > 'z') {
			return false
		}
	}
	return true
}

func safeStack(skip int) *sentry.Stacktrace {
	pcs := make([]uintptr, 24)
	n := runtime.Callers(skip+2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	result := make([]sentry.Frame, 0, n)
	for {
		frame, more := frames.Next()
		function := frame.Function
		if slash := strings.LastIndex(function, "/"); slash >= 0 {
			function = function[slash+1:]
		}
		result = append(result, sentry.Frame{Function: function, Filename: filepath.Base(frame.File), Lineno: frame.Line, InApp: true})
		if !more {
			break
		}
	}
	return &sentry.Stacktrace{Frames: result}
}

func sanitize(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event == nil || len(event.Exception) != 1 || !safeClassification(event.Exception[0].Type) {
		return nil
	}
	tags := map[string]string{}
	for _, key := range []string{"component", "support_reference", "service_component"} {
		if value := event.Tags[key]; value != "" {
			tags[key] = value
		}
	}
	var frames []sentry.Frame
	if stack := event.Exception[0].Stacktrace; stack != nil {
		for _, frame := range stack.Frames {
			frames = append(frames, sentry.Frame{Function: frame.Function, Filename: filepath.Base(frame.Filename), Lineno: frame.Lineno, InApp: true})
		}
	}
	return &sentry.Event{EventID: event.EventID, Timestamp: event.Timestamp, Level: sentry.LevelError, Release: event.Release, Environment: event.Environment, Contexts: safeTraceContext(event.Contexts), Platform: "go", Tags: tags, Exception: []sentry.Exception{{Type: event.Exception[0].Type, Stacktrace: &sentry.Stacktrace{Frames: frames}}}}
}

func signalFlag(name string, defaultValue bool) (bool, bool) {
	switch os.Getenv(name) {
	case "":
		return defaultValue, true
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}
