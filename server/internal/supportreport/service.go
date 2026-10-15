// Package supportreport owns bounded, content-free support evidence. References
// are correlation hints; ownership and operator authority are separate.
package supportreport

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/google/uuid"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
)

const QueueCapacity = 128
const Retention = 7 * 24 * time.Hour

var ErrInvalid = errors.New("invalid support report")
var ErrUnavailable = errors.New("support evidence unavailable")
var referencePattern = regexp.MustCompile(`^support_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var tokenPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

func ValidReference(v string) bool { return referencePattern.MatchString(v) }
func NewReference() string         { return "support_" + uuid.NewString() }

type Config struct {
	Enabled                     bool
	DSN, Release, Environment   string
	Region, Instance            string
	LogsEnabled, MetricsEnabled *bool
	TracesSampleRate            *float64
	PlatformMetricNames         []string
}
type Record struct {
	ID             string    `json:"id"`
	Reference      string    `json:"support_reference"`
	OwnerID        string    `json:"owner_id,omitempty"`
	RequestID      string    `json:"request_id,omitempty"`
	Component      string    `json:"component"`
	Operation      string    `json:"operation"`
	Release        string    `json:"release"`
	Stage          string    `json:"stage"`
	Code           string    `json:"code"`
	At             time.Time `json:"at"`
	ExpiresAt      time.Time `json:"expires_at"`
	EventID        string    `json:"sentry_event_id,omitempty"`
	Reporting      string    `json:"reporting"`
	SourceFile     string    `json:"source_file,omitempty"`
	SourceFunction string    `json:"source_function,omitempty"`
	SourceLine     int       `json:"source_line,omitempty"`
	CorrelationID  string    `json:"correlation_id,omitempty"`
	Cause          string    `json:"cause"`
	ErrorType      string    `json:"error_type"`
	ErrorChain     []string  `json:"error_chain,omitempty"`
	Errno          int       `json:"errno,omitempty"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	Outcome        string    `json:"outcome"`
	Severity       string    `json:"severity"`
}
type Store interface {
	Save(context.Context, Record) error
	Lookup(context.Context, string, string, string, bool) ([]Record, error)
	Cleanup(context.Context) error
}
type item struct {
	record     Record
	fault      Fault
	stack      *sentry.Stacktrace
	unexpected bool
	trace      sentry.Context
}
type Service struct {
	store                 Store
	client                *sentry.Client
	release               string
	environment           string
	region, instance      string
	queue                 chan item
	dropped               atomic.Uint64
	mu                    sync.Mutex
	admission             sync.RWMutex
	window                time.Time
	count                 int
	signals               *signalState
	logger                *slog.Logger
	cleanupFailures       atomic.Uint64
	storeFailures         atomic.Uint64
	sdkHTTPFailures       atomic.Uint64
	sdkSubmissionsDropped atomic.Uint64
	flushStatus           atomic.Uint32
	deliveryMu            sync.Mutex
	deliveryFailed        bool
	deliveryClass         string
	storeFailureMu        sync.Mutex
	storeFailed           bool
	storeFailureClass     string
	closed                atomic.Bool
	closeOnce             sync.Once
}

const (
	flushNotStarted uint32 = iota
	flushInProgress
	flushDrained
	flushTimedOut
)

type sentryHTTPRoundTripper struct {
	service *Service
	base    http.RoundTripper
}

func (t sentryHTTPRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	if err != nil || response == nil || status >= http.StatusBadRequest {
		t.service.observeSentryDeliveryFailure(status, err)
	} else {
		t.service.observeSentryDeliverySuccess()
	}
	return response, err
}

func (s *Service) beginAdmission() bool {
	if s == nil {
		return false
	}
	s.admission.RLock()
	if s.closed.Load() {
		s.admission.RUnlock()
		return false
	}
	return true
}

func (s *Service) endAdmission() {
	s.admission.RUnlock()
}

func (s *Service) stopAdmission() {
	if s == nil {
		return
	}
	s.admission.Lock()
	s.closed.Store(true)
	s.admission.Unlock()
}

// ProjectFaultWithStack projects only safe source identity from the captured
// Go stack. The stack body itself is never persisted or logged locally.
func ProjectFaultWithStack(ctx context.Context, component, operation, stage, code string, err error, stack *sentry.Stacktrace) Fault {
	fault := ProjectFault(ctx, component, operation, stage, code, err)
	fault.SourceFile, fault.SourceFunction, fault.SourceLine = sourceLocation(stack)
	return fault
}

// unavailableError keeps the stable API sentinel and the original cause
// traversable without including the cause's text in its Error method.
type unavailableError struct{ cause error }

func (unavailableError) Error() string { return ErrUnavailable.Error() }
func (e unavailableError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrUnavailable}
	}
	return []error{ErrUnavailable, e.cause}
}

func New(store Store, cfg Config) (*Service, error) {
	if store == nil || !ValidFleet(cfg.Region, cfg.Instance) {
		return nil, ErrInvalid
	}
	release := cfg.Release
	if release == "" {
		release = "development"
	}
	if !tokenPattern.MatchString(release) {
		return nil, ErrInvalid
	}
	s := &Service{store: store, release: release, environment: cfg.Environment, region: cfg.Region, instance: cfg.Instance, queue: make(chan item, QueueCapacity), logger: slog.Default()}
	options := sentry.ClientOptions{}
	if err := s.configureSignals(cfg, &options); err != nil {
		return nil, err
	}
	if cfg.Enabled {
		if cfg.DSN == "" || cfg.Release == "" || !tokenPattern.MatchString(cfg.Environment) {
			return nil, ErrInvalid
		}
		transport := sentry.NewHTTPTransport()
		transport.BufferSize = 32
		transport.Timeout = 2 * time.Second
		options.Dsn, options.Release, options.Environment = cfg.DSN, release, cfg.Environment
		options.Transport, options.MaxBreadcrumbs = transport, -1
		//paperboat:allow-source-policy default-http owner=supportreport reason=sdk_http_client_timeout_2s
		options.HTTPTransport = sentryHTTPRoundTripper{service: s, base: http.DefaultTransport}
		options.Integrations = func([]sentry.Integration) []sentry.Integration { return nil }
		options.BeforeSend = func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			out := sanitize(e)
			if out != nil {
				s.fleetTags(out.Tags)
			}
			return out
		}
		client, err := sentry.NewClient(options)
		if err != nil {
			return nil, ErrInvalid
		}
		s.client = client
	}
	return s, nil
}

// Capture never performs disk or network I/O on the product path. Both evidence
// and Sentry work share a bounded queue; lost evidence is never called delivered.
func (s *Service) Capture(reference, owner, request, stage, code string, unexpected bool, stack *sentry.Stacktrace) {
	s.CaptureContext(context.Background(), reference, owner, request, stage, code, unexpected, stack)
}
func (s *Service) CaptureContext(ctx context.Context, reference, owner, request, stage, code string, unexpected bool, stack *sentry.Stacktrace) {
	if s == nil || !ValidReference(reference) || !tokenPattern.MatchString(stage) || !tokenPattern.MatchString(code) {
		return
	}
	if !knownStage(stage) {
		stage = "other"
	}
	if !knownFaultCode(code) {
		code = "internal_error"
	}
	if !tokenPattern.MatchString(request) {
		request = ""
	}
	if owner != "" && !tokenPattern.MatchString(owner) {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	if now.Sub(s.window) >= time.Minute {
		s.window = now
		s.count = 0
	}
	if s.count >= 120 {
		s.mu.Unlock()
		s.dropped.Add(1)
		s.sdkSubmissionsDropped.Add(1)
		return
	}
	s.count++
	s.mu.Unlock()
	faultCtx := s.Context(ctx, reference)
	faultCtx = WithFailureIdentity(faultCtx, owner, request)
	fault := ProjectFaultWithStack(faultCtx, "paperboat-server", "http_other", stage, code, nil, stack)
	fault.Outcome, fault.Severity = "failed", "error"
	if !unexpected {
		fault.Outcome, fault.Severity = "rejected", "warning"
	}
	r := recordFromFault(fault, NewReference(), owner, request, s.release, now)
	r.SourceFile, r.SourceFunction, r.SourceLine = sourceLocation(stack)
	if s.client != nil && unexpected {
		r.Reporting = "unconfirmed"
		r.EventID = strings.ReplaceAll(uuid.NewString(), "-", "")
	} else if s.client != nil {
		r.Reporting = "filtered"
	}
	var trace sentry.Context
	if span := sentry.SpanFromContext(ctx); span != nil {
		trace = sentry.Context{"trace_id": span.TraceID, "span_id": span.SpanID}
	}
	if !s.beginAdmission() {
		s.dropped.Add(1)
		s.sdkSubmissionsDropped.Add(1)
		return
	}
	select {
	case s.queue <- item{record: r, fault: fault, stack: cleanStack(stack), unexpected: unexpected, trace: trace}:
	default:
		s.dropped.Add(1)
		s.sdkSubmissionsDropped.Add(1)
	}
	s.endAdmission()
}

// SetLogger installs the application-owned structured logger used by reporter
// maintenance failures. It is called during composition, before Run starts.
func (s *Service) SetLogger(logger *slog.Logger) {
	if s != nil && logger != nil {
		s.logger = logger
	}
}

// LogFault emits the same bounded fault projection returned by CaptureFailure.
func (s *Service) LogFault(ctx context.Context, level slog.Level, message string, fault Fault) {
	s.logFault(ctx, level, message, fault)
}

// CaptureFailure projects the original wrapped error once, records only its
// bounded metadata, and returns the exact safe fields callers should log.
func (s *Service) CaptureFailure(ctx context.Context, component, operation, stage, code string, err error) Fault {
	return s.CaptureFailureWithStack(ctx, component, operation, stage, code, err, nil)
}

// CaptureFromContext uses only the reporter explicitly bound to this request
// by Service.Context/Begin. If there is no request-owned binding it returns a
// safe local projection and writes it to the configured default structured
// logger; it never falls back to another process reporter.
func CaptureFromContext(ctx context.Context, component, operation, stage, code string, err error) Fault {
	if ctx == nil {
		ctx = context.Background()
	}
	if sc, ok := ctx.Value(signalContextKey{}).(signalContext); ok && sc.service != nil {
		fault := sc.service.CaptureFailureWithStack(ctx, component, operation, stage, code, err, sentry.NewStacktrace())
		sc.service.logFault(ctx, faultLogLevel(fault), "operation failed", fault)
		return fault
	}
	fault := ProjectFaultWithStack(ctx, component, operation, stage, code, err, sentry.NewStacktrace())
	slog.Default().Log(ctx, faultLogLevel(fault), "operation failed", fault.LogAttrs()...)
	return fault
}

// ObserveFromContext records and logs a bounded failure attempt without
// submitting a Sentry exception. It is for work whose public operation has
// already committed and whose retry owner is responsible for eventual
// recovery. Like CaptureFromContext, it uses only a reporter explicitly bound
// to this request.
func ObserveFromContext(ctx context.Context, component, operation, stage, code string, err error) Fault {
	if ctx == nil {
		ctx = context.Background()
	}
	if sc, ok := ctx.Value(signalContextKey{}).(signalContext); ok && sc.service != nil {
		fault := sc.service.ObserveFailureWithStack(ctx, component, operation, stage, code, err, sentry.NewStacktrace())
		sc.service.logFault(ctx, faultLogLevel(fault), "operation partially failed", fault)
		return fault
	}
	fault := ProjectFaultWithStack(ctx, component, operation, stage, code, err, sentry.NewStacktrace())
	fault = observedFault(fault)
	slog.Default().Log(ctx, faultLogLevel(fault), "operation partially failed", fault.LogAttrs()...)
	return fault
}

func observedFault(fault Fault) Fault {
	if fault.Cause == "context_canceled" {
		fault.Outcome, fault.Severity = "canceled", "info"
	} else {
		fault.Outcome, fault.Severity = "failed", "warning"
	}
	return fault
}

func faultLogLevel(fault Fault) slog.Level {
	switch fault.Severity {
	case "info":
		return slog.LevelInfo
	case "warning":
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

// CaptureFailureWithStack preserves a stack captured by the actual panic or
// error boundary while retaining CaptureFailure's privacy projection.
func (s *Service) CaptureFailureWithStack(ctx context.Context, component, operation, stage, code string, err error, stack *sentry.Stacktrace) Fault {
	if ctx == nil {
		ctx = context.Background()
	}
	reference := ""
	if sc, ok := ctx.Value(signalContextKey{}).(signalContext); ok && sc.service == s && ValidReference(sc.reference) {
		reference = sc.reference
	} else if identity, ok := ctx.Value(identityContextKey{}).(faultIdentity); ok && ValidReference(identity.reference) {
		reference = identity.reference
	}
	if !ValidReference(reference) && s != nil {
		reference = NewReference()
		ctx = s.Context(ctx, reference)
	}
	if stack == nil && err != nil {
		stack = sentry.NewStacktrace()
	}
	fault := ProjectFaultWithStack(ctx, component, operation, stage, code, err, stack)
	if err == nil && code == "worker_recovered" {
		fault.Outcome, fault.Severity = "success", "info"
	}
	if s == nil || !ValidReference(fault.SupportReference) {
		return fault
	}
	return s.enqueueFault(ctx, fault, stack, fault.Severity == "error" && fault.Outcome == "failed")
}

// ObserveFailure records a classified attempt locally without creating an
// exception event. Callers use it after a durable response when a bounded
// retry owner will continue the operation.
func (s *Service) ObserveFailure(ctx context.Context, component, operation, stage, code string, err error) Fault {
	return s.ObserveFailureWithStack(ctx, component, operation, stage, code, err, nil)
}

// ObserveFailureWithStack preserves the failure boundary while keeping the
// observation local to support evidence and structured logs.
func (s *Service) ObserveFailureWithStack(ctx context.Context, component, operation, stage, code string, err error, stack *sentry.Stacktrace) Fault {
	if ctx == nil {
		ctx = context.Background()
	}
	if stack == nil && err != nil {
		stack = sentry.NewStacktrace()
	}
	reference := ""
	if sc, ok := ctx.Value(signalContextKey{}).(signalContext); ok && sc.service == s && ValidReference(sc.reference) {
		reference = sc.reference
	} else if identity, ok := ctx.Value(identityContextKey{}).(faultIdentity); ok && ValidReference(identity.reference) {
		reference = identity.reference
	}
	if !ValidReference(reference) && s != nil {
		reference = NewReference()
		ctx = s.Context(ctx, reference)
	}
	fault := ProjectFaultWithStack(ctx, component, operation, stage, code, err, stack)
	fault = observedFault(fault)
	if s == nil || !ValidReference(fault.SupportReference) {
		return fault
	}
	return s.enqueueFault(ctx, fault, stack, false)
}

func (s *Service) enqueueFault(ctx context.Context, fault Fault, stack *sentry.Stacktrace, unexpected bool) Fault {
	now := time.Now().UTC()
	identity, _ := ctx.Value(identityContextKey{}).(faultIdentity)
	owner := identity.owner
	request := serverdiagnostics.NormalizeRequestID(identity.request)
	s.mu.Lock()
	if now.Sub(s.window) >= time.Minute {
		s.window = now
		s.count = 0
	}
	if s.count >= 120 {
		s.mu.Unlock()
		s.dropped.Add(1)
		s.sdkSubmissionsDropped.Add(1)
		return fault
	}
	s.count++
	s.mu.Unlock()
	record := recordFromFault(fault, NewReference(), owner, request, s.release, now)
	record.SourceFile, record.SourceFunction, record.SourceLine = sourceLocation(stack)
	if s.client != nil && unexpected {
		record.Reporting = "unconfirmed"
		record.EventID = strings.ReplaceAll(uuid.NewString(), "-", "")
	} else if s.client != nil {
		record.Reporting = "filtered"
	}
	var trace sentry.Context
	if span := sentry.SpanFromContext(ctx); span != nil {
		trace = sentry.Context{"trace_id": span.TraceID, "span_id": span.SpanID}
	}
	if !s.beginAdmission() {
		s.dropped.Add(1)
		s.sdkSubmissionsDropped.Add(1)
		return fault
	}
	select {
	case s.queue <- item{record: record, fault: fault, stack: cleanStack(stack), unexpected: unexpected, trace: trace}:
	default:
		s.dropped.Add(1)
		s.sdkSubmissionsDropped.Add(1)
	}
	s.endAdmission()
	return fault
}

func recordFromFault(fault Fault, id, owner, request, release string, at time.Time) Record {
	return Record{
		ID: id, Reference: fault.SupportReference, OwnerID: owner, RequestID: request,
		Component: fault.Component, Operation: fault.Operation, Release: release,
		Stage: fault.Stage, Code: fault.Code, At: at, ExpiresAt: at.Add(Retention),
		Reporting: "disabled", CorrelationID: fault.CorrelationID, Cause: fault.Cause,
		ErrorType: fault.ErrorType, ErrorChain: append([]string(nil), fault.ErrorChain...),
		Errno: fault.Errno, HTTPStatus: fault.HTTPStatus,
		SourceFile: fault.SourceFile, SourceFunction: fault.SourceFunction, SourceLine: fault.SourceLine,
		Outcome: fault.Outcome, Severity: fault.Severity,
	}
}

func (s *Service) Dropped() uint64 { return s.dropped.Load() }
func (s *Service) SDKHTTPFailures() uint64 {
	if s == nil {
		return 0
	}
	return s.sdkHTTPFailures.Load()
}
func (s *Service) SDKSubmissionsDropped() uint64 {
	if s == nil {
		return 0
	}
	return s.sdkSubmissionsDropped.Load()
}

// FlushStatus reports local SDK shutdown progress, not proof of delivery.
func (s *Service) FlushStatus() string {
	if s == nil || s.client == nil {
		return "disabled"
	}
	switch s.flushStatus.Load() {
	case flushInProgress:
		return "flushing"
	case flushDrained:
		return "drained"
	case flushTimedOut:
		return "timed_out"
	default:
		return "not_started"
	}
}
func (s *Service) CleanupFailures() uint64 {
	if s == nil {
		return 0
	}
	return s.cleanupFailures.Load()
}
func (s *Service) StoreFailures() uint64 {
	if s == nil {
		return 0
	}
	return s.storeFailures.Load()
}
func (s *Service) Lookup(ctx context.Context, actor, owner, ref string, operator bool) ([]Record, error) {
	if !ValidReference(ref) || !tokenPattern.MatchString(actor) || (owner != "" && !tokenPattern.MatchString(owner)) || (!operator && actor != owner) {
		return nil, ErrInvalid
	}
	records, err := s.store.Lookup(ctx, actor, owner, ref, operator)
	if err != nil {
		return nil, unavailableError{cause: err}
	}
	return records, nil
}
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	snapshotTicker := time.NewTicker(snapshotInterval)
	defer snapshotTicker.Stop()
	s.snapshotMetrics()
	cleanupReference := NewReference()
	cleanupFailed := false
	for {
		if ctx.Err() != nil {
			return s.drainShutdown()
		}
		select {
		case <-ctx.Done():
			return s.drainShutdown()
		case <-snapshotTicker.C:
			s.snapshotMetrics()
		case <-ticker.C:
			c, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := s.store.Cleanup(c)
			cancel()
			s.observeCleanup(ctx, cleanupReference, &cleanupFailed, err)
		case value := <-s.queue:
			// Once dequeued, persistence owns its bounded lifetime even if Run stops.
			s.process(context.WithoutCancel(ctx), value)
		}
	}
}

func (s *Service) drainShutdown() error {
	s.snapshotMetrics()
	s.stopAdmission()
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer s.close(shutdown)
	for shutdown.Err() == nil {
		select {
		case value := <-s.queue:
			s.process(shutdown, value)
		default:
			return nil
		}
	}
	s.dropped.Add(uint64(len(s.queue)))
	return nil
}

func (s *Service) observeCleanup(ctx context.Context, reference string, failed *bool, err error) {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return
	}
	if err != nil {
		s.cleanupFailures.Add(1)
		if !*failed {
			faultCtx := s.Context(ctx, reference)
			fault := s.CaptureFailure(faultCtx, "paperboat-server", "worker_reporting_cleanup", "report_cleanup", "support_cleanup_failed", err)
			s.logFault(ctx, slog.LevelError, "support report cleanup failed", fault)
		}
		*failed = true
		return
	}
	if *failed {
		fault := s.CaptureFailure(s.Context(ctx, reference), "paperboat-server", "worker_reporting_cleanup", "report_cleanup", "worker_recovered", nil)
		s.logFault(ctx, slog.LevelInfo, "support report cleanup recovered", fault)
		*failed = false
	}
}

func (s *Service) process(ctx context.Context, value item) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := s.store.Save(c, value.record)
	cancel()
	if err != nil {
		s.dropped.Add(1)
		s.observeStoreFailure(ctx, value.record, err)
	} else {
		s.observeStoreRecovery(ctx, value.record)
	}
	if s.client == nil || !value.unexpected {
		return
	}
	e := NewFaultEvent(value.fault, value.stack)
	if e == nil {
		s.sdkSubmissionsDropped.Add(1)
		return
	}
	e.EventID, e.Timestamp, e.Release = sentry.EventID(value.record.EventID), value.record.At, s.release
	if value.trace != nil {
		if e.Contexts == nil {
			e.Contexts = make(map[string]sentry.Context)
		}
		e.Contexts["trace"] = value.trace
	}
	if s.client.CaptureEvent(e, nil, nil) == nil {
		s.sdkSubmissionsDropped.Add(1)
	}
}

func storeFaultContext(ctx context.Context, record Record) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = serverdiagnostics.WithCorrelationID(ctx, record.CorrelationID)
	ctx = WithFailureIdentity(ctx, record.OwnerID, record.RequestID, record.Reference)
	return ctx
}

func (s *Service) observeStoreFailure(ctx context.Context, record Record, err error) {
	s.storeFailures.Add(1)
	faultCtx := s.Context(ctx, record.Reference)
	faultCtx = storeFaultContext(faultCtx, record)
	fault := ProjectFault(faultCtx, "paperboat-server", "worker_reporting_storage", "service_error", "support_storage_failed", err)
	class := strings.Join([]string{fault.Cause, fault.ErrorType, strconv.Itoa(fault.Errno), strconv.Itoa(fault.HTTPStatus)}, "\x00")
	s.storeFailureMu.Lock()
	changed := !s.storeFailed || class != s.storeFailureClass
	s.storeFailed, s.storeFailureClass = true, class
	s.storeFailureMu.Unlock()
	if changed {
		s.logFault(ctx, slog.LevelError, "support report persistence failed", fault)
	}
}

func (s *Service) observeStoreRecovery(ctx context.Context, record Record) {
	s.storeFailureMu.Lock()
	wasFailed := s.storeFailed
	s.storeFailed, s.storeFailureClass = false, ""
	s.storeFailureMu.Unlock()
	if !wasFailed {
		return
	}
	faultCtx := storeFaultContext(s.Context(ctx, record.Reference), record)
	fault := ProjectFault(faultCtx, "paperboat-server", "worker_reporting_storage", "service_error", "worker_recovered", nil)
	s.logFault(ctx, slog.LevelInfo, "support report persistence recovered", fault)
}

func (s *Service) logFault(ctx context.Context, level slog.Level, message string, fault Fault) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Log(ctx, level, message, fault.LogAttrs()...)
}

func faultTags(fault Fault) map[string]string {
	tags := map[string]string{
		"support_reference": fault.SupportReference,
		"component":         fault.Component,
		"operation":         fault.Operation,
		"stage":             fault.Stage,
		"code":              fault.Code,
		"cause":             fault.Cause,
		"error_type":        fault.ErrorType,
		"source_file":       fault.SourceFile,
		"source_function":   fault.SourceFunction,
		"source_line":       strconv.Itoa(fault.SourceLine),
		"correlation_id":    fault.CorrelationID,
		"outcome":           fault.Outcome,
		"severity":          fault.Severity,
		"errno":             strconv.Itoa(fault.Errno),
	}
	if fault.HTTPStatus >= 400 && fault.HTTPStatus <= 599 {
		tags["http_status"] = strconv.Itoa(fault.HTTPStatus)
	}
	return tags
}

func faultContext(fault Fault) sentry.Context {
	chain := append([]string(nil), fault.ErrorChain...)
	if len(chain) > maxErrorChain {
		chain = chain[:maxErrorChain]
	}
	ctx := sentry.Context{"error_chain": chain, "errno": fault.Errno, "outcome": fault.Outcome, "severity": fault.Severity}
	if fault.HTTPStatus >= 400 && fault.HTTPStatus <= 599 {
		ctx["http_status"] = fault.HTTPStatus
	}
	return ctx
}
func cleanStack(stack *sentry.Stacktrace) *sentry.Stacktrace {
	if stack == nil {
		return nil
	}
	out := &sentry.Stacktrace{}
	for _, f := range stack.Frames {
		if len(out.Frames) >= 64 {
			break
		}
		line := f.Lineno
		if line < 1 || line > 1_000_000 {
			line = 0
		}
		out.Frames = append(out.Frames, sentry.Frame{Function: safeSourceFunction(f.Function), Module: safeSourceFunction(f.Module), Filename: safeSourceFile(frameFilename(f)), Lineno: line, InApp: f.InApp})
	}
	return out
}
func sanitize(e *sentry.Event) *sentry.Event {
	// Rebuild instead of trying to blacklist an evolving SDK event schema.
	if !ValidReference(e.Tags["support_reference"]) || e.Tags["component"] != "paperboat-server" || !validOperation(e.Tags["operation"]) || !knownFaultCode(e.Tags["code"]) || !knownStage(e.Tags["stage"]) || !knownCause(e.Tags["cause"]) || safeErrorTypeValue(e.Tags["error_type"]) == "" || !validFaultOutcome(e.Tags["outcome"]) || !validFaultSeverity(e.Tags["severity"]) {
		return nil
	}
	if ref := e.Tags["correlation_id"]; ref != "" && serverdiagnostics.NormalizeRequestID(ref) == "" {
		return nil
	}
	if file := e.Tags["source_file"]; file != "" && safeSourceFile(file) != file {
		return nil
	}
	if function := e.Tags["source_function"]; function != "" && safeSourceFunction(function) != function {
		return nil
	}
	if line := e.Tags["source_line"]; line != "" {
		value, err := strconv.Atoi(line)
		if err != nil || value < 0 || value > 1_000_000 {
			return nil
		}
	}
	errno := 0
	if e.Tags["errno"] != "" {
		for _, digit := range e.Tags["errno"] {
			if digit < '0' || digit > '9' {
				return nil
			}
			errno = errno*10 + int(digit-'0')
			if errno > 65535 {
				return nil
			}
		}
	}
	out := &sentry.Event{EventID: e.EventID, Timestamp: e.Timestamp, Level: sentry.LevelError, Platform: "go", Release: e.Release, Environment: e.Environment, Tags: map[string]string{}, Fingerprint: []string{"{{ default }}", "paperboat-server", e.Tags["operation"], e.Tags["stage"], e.Tags["code"], e.Tags["cause"]}}
	if trace := safeTraceContext(e.Contexts["trace"]); len(trace) > 0 {
		out.Contexts = map[string]sentry.Context{"trace": trace}
	}
	if fault := safeFaultContext(e.Contexts["fault"]); len(fault) > 0 {
		if out.Contexts == nil {
			out.Contexts = map[string]sentry.Context{}
		}
		out.Contexts["fault"] = fault
	}
	if errno > 0 && out.Contexts != nil {
		if fault, ok := out.Contexts["fault"]; ok {
			fault["errno"] = errno
		}
	}
	for _, k := range []string{"support_reference", "component", "operation", "stage", "code", "cause", "error_type", "source_file", "source_function", "source_line", "correlation_id", "outcome", "severity", "errno", "http_status"} {
		if k == "http_status" && e.Tags[k] == "" {
			continue
		}
		out.Tags[k] = e.Tags[k]
	}
	if out.Tags["http_status"] != "" {
		status, err := strconv.Atoi(out.Tags["http_status"])
		if err != nil || status < 400 || status > 599 {
			return nil
		}
	}
	for i, ex := range e.Exception {
		if i >= 1 {
			break
		}
		out.Exception = append(out.Exception, sentry.Exception{Type: e.Tags["error_type"], Value: e.Tags["cause"], Stacktrace: cleanStack(ex.Stacktrace)})
	}
	return out
}

func safeFaultContext(value sentry.Context) sentry.Context {
	chain, ok := value["error_chain"].([]string)
	if !ok || len(chain) > maxErrorChain {
		return nil
	}
	clean := make([]string, 0, len(chain))
	for _, name := range chain {
		if safeErrorTypeValue(name) == "" {
			return nil
		}
		clean = append(clean, name)
	}
	errno, ok := value["errno"].(int)
	if !ok || errno < 0 || errno > 65535 {
		return nil
	}
	outcome, _ := value["outcome"].(string)
	severity, _ := value["severity"].(string)
	if !validFaultOutcome(outcome) || !validFaultSeverity(severity) {
		return nil
	}
	var status int
	if rawStatus, present := value["http_status"]; present {
		var ok bool
		status, ok = rawStatus.(int)
		if !ok || status < 400 || status > 599 {
			return nil
		}
	}
	out := sentry.Context{"error_chain": clean, "errno": errno, "outcome": outcome, "severity": severity}
	if status != 0 {
		out["http_status"] = status
	}
	return out
}

// Source identity is obtained from our own stack, never an error message.
func sourceLocation(stack *sentry.Stacktrace) (string, string, int) {
	if stack == nil {
		return "", "", 0
	}
	for i := len(stack.Frames) - 1; i >= 0; i-- {
		f := stack.Frames[i]
		if !strings.HasPrefix(f.Module, "github.com/pinksaucepasta/paperboat-server/") || strings.Contains(f.Module, "/supportreport") {
			continue
		}
		function := safeSourceFunction(f.Function)
		if sourceWrapper(function) {
			continue
		}
		return safeSourceFile(frameFilename(f)), safeSourceFunction(f.Function), safeSourceLine(f.Lineno)
	}
	return "", "", 0
}

func sourceWrapper(function string) bool {
	for _, name := range []string{"captureSupportFailure", "writeErrorDetails", "writeError", "writePanicError", "writePreviewTunnelError", "writePreviewTunnelErrorWithValues", "writeAttachmentServiceError"} {
		if function == name || strings.HasSuffix(function, "."+name) {
			return true
		}
	}
	return false
}

func safeSourceLine(line int) int {
	if line < 1 || line > 1_000_000 {
		return 0
	}
	return line
}

func frameFilename(frame sentry.Frame) string {
	path := frame.Filename
	if path == "" {
		path = frame.AbsPath
	}
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}

func (s *Service) close(ctx context.Context) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.stopAdmission()
		if s.client != nil {
			s.flushStatus.Store(flushInProgress)
			if s.client.FlushWithContext(ctx) {
				s.flushStatus.Store(flushDrained)
				s.logFlushStatus(ctx, slog.LevelInfo, "Sentry reporting shutdown drained", "drained")
			} else {
				s.flushStatus.Store(flushTimedOut)
				fault := ProjectFault(context.Background(), "paperboat-server", "service_lifecycle", "service_shutdown", "support_unavailable", context.DeadlineExceeded)
				s.logger.Log(context.Background(), slog.LevelWarn, "Sentry reporting flush timed out", append(fault.LogAttrs(), s.flushLogAttrs("timed_out")...)...)
			}
			s.client.Close()
		}
	})
}

func (s *Service) flushLogAttrs(status string) []any {
	return []any{
		"flush_status", status,
		"sdk_http_failures", s.SDKHTTPFailures(),
		"sdk_submissions_dropped", s.SDKSubmissionsDropped(),
		"delivery", "unconfirmed",
	}
}

func (s *Service) logFlushStatus(ctx context.Context, level slog.Level, message, status string) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Log(ctx, level, message, s.flushLogAttrs(status)...)
}

func (s *Service) observeSentryDeliveryFailure(status int, err error) {
	if s == nil {
		return
	}
	s.sdkHTTPFailures.Add(1)
	cause := err
	if status >= http.StatusBadRequest && status <= 599 {
		cause = WithDiagnosticStatus(status, err)
	}
	fault := ProjectFault(context.Background(), "paperboat-server", "service_lifecycle", "service_error", "sentry_delivery_failed", cause)
	class := strings.Join([]string{fault.Cause, fault.ErrorType, strconv.Itoa(fault.Errno), strconv.Itoa(fault.HTTPStatus)}, "\x00")
	s.deliveryMu.Lock()
	changed := !s.deliveryFailed || class != s.deliveryClass
	s.deliveryFailed, s.deliveryClass = true, class
	s.deliveryMu.Unlock()
	if changed {
		s.logFault(context.Background(), slog.LevelWarn, "Sentry delivery failed", fault)
	}
}

func (s *Service) observeSentryDeliverySuccess() {
	if s == nil {
		return
	}
	s.deliveryMu.Lock()
	wasFailed := s.deliveryFailed
	s.deliveryFailed, s.deliveryClass = false, ""
	s.deliveryMu.Unlock()
	if wasFailed {
		fault := ProjectFault(context.Background(), "paperboat-server", "service_lifecycle", "service_error", "worker_recovered", nil)
		s.logFault(context.Background(), slog.LevelInfo, "Sentry delivery recovered", fault)
	}
}

// Close releases reporters whose application construction failed before Run.
func (s *Service) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.close(ctx)
}
