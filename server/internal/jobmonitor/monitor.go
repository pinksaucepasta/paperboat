// Package jobmonitor sends content-free scheduled-job check-ins. The remote
// monitor owns the schedule and detects missed starts and unfinished runs.
package jobmonitor

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

var ErrConfig = errors.New("invalid scheduled monitoring configuration")
var slug = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var label = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

type Config struct {
	Enabled                         bool
	DSN, Release, Environment, Slug string
	Logger                          *slog.Logger
}
type Monitor struct {
	client             *sentry.Client
	slug               string
	logger             *slog.Logger
	httpFailures       atomic.Uint64
	submissionsDropped atomic.Uint64
	flushStatus        atomic.Uint32
	deliveryMu         sync.Mutex
	deliveryFailed     bool
	deliveryClass      string
}

const (
	flushNotStarted uint32 = iota
	flushInProgress
	flushDrained
	flushTimedOut
)

type deliveryTransport struct {
	monitor *Monitor
	base    http.RoundTripper
}

func (t deliveryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	status := 0
	if response != nil {
		status = response.StatusCode
	} else if err == nil {
		err = errors.New("Sentry endpoint returned no response")
	}
	if err != nil || response == nil || status >= http.StatusBadRequest {
		t.monitor.observeDeliveryFailure(status, err)
	} else {
		t.monitor.observeDeliverySuccess()
	}
	return response, err
}

func New(c Config) (*Monitor, error) {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if !c.Enabled {
		return &Monitor{logger: logger}, nil
	}
	if c.DSN == "" || !slug.MatchString(c.Slug) || !label.MatchString(c.Release) || !label.MatchString(c.Environment) {
		return nil, ErrConfig
	}
	m := &Monitor{slug: c.Slug, logger: logger}
	transport := sentry.NewHTTPTransport()
	transport.BufferSize, transport.Timeout = 4, 2*time.Second
	//paperboat:allow-source-policy default-http owner=jobmonitor reason=sdk_http_client_timeout_2s
	client, err := sentry.NewClient(sentry.ClientOptions{Dsn: c.DSN, Release: c.Release, Environment: c.Environment, Transport: transport, HTTPTransport: deliveryTransport{monitor: m, base: http.DefaultTransport}, MaxBreadcrumbs: -1,
		Integrations: func([]sentry.Integration) []sentry.Integration { return nil },
		BeforeSend:   cleanBackupEvent,
	})
	if err != nil {
		return nil, ErrConfig
	}
	m.client = client
	return m, nil
}

// FromEnv is explicit: enabled monitoring requires a configured monitor for the
// selected command. No hostname, DB name, error text or command is transmitted.
func FromEnv(monitorVariable string) (*Monitor, error) {
	enabled := os.Getenv("PAPERBOAT_SENTRY_ENABLED")
	if enabled != "" && enabled != "false" && enabled != "true" {
		return nil, ErrConfig
	}
	return New(Config{Enabled: enabled == "true", DSN: os.Getenv("PAPERBOAT_SENTRY_DSN"), Release: os.Getenv("PAPERBOAT_SENTRY_RELEASE"), Environment: os.Getenv("PAPERBOAT_SENTRY_ENVIRONMENT"), Slug: os.Getenv(monitorVariable)})
}

// Start returns an idempotent completion function. A successful flush only means
// the local SDK drained; remote receipt is verified through the admin API.
func (m *Monitor) Start() func(bool) {
	if m == nil || m.client == nil {
		return func(bool) {}
	}
	started := time.Now()
	id := m.client.CaptureCheckIn(&sentry.CheckIn{MonitorSlug: m.slug, Status: sentry.CheckInStatusInProgress}, nil, nil)
	if id == nil {
		m.submissionsDropped.Add(1)
		m.logDeliveryFailure(errors.New("scheduled check-in was not admitted"))
	}
	m.flush(false)
	var once sync.Once
	return func(ok bool) {
		once.Do(func() {
			if id == nil {
				return
			}
			status := sentry.CheckInStatusError
			if ok {
				status = sentry.CheckInStatusOK
			}
			m.client.CaptureCheckIn(&sentry.CheckIn{ID: *id, MonitorSlug: m.slug, Status: status, Duration: time.Since(started)}, nil, nil)
			m.flush(false)
		})
	}
}

func (m *Monitor) Close() {
	if m != nil && m.client != nil {
		m.flush(true)
		status := m.FlushStatus()
		m.logger.Log(context.Background(), slog.LevelInfo, "Sentry job monitor shutdown complete", "component", "paperboat-server", "operation", "service_watchdog", "stage", "service_shutdown", "flush_status", status, "sdk_http_failures", m.httpFailures.Load(), "sdk_submissions_dropped", m.submissionsDropped.Load(), "delivery", "unconfirmed")
		m.client.Close()
	}
}

func (m *Monitor) FlushStatus() string {
	if m == nil || m.client == nil {
		return "disabled"
	}
	switch m.flushStatus.Load() {
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

func (m *Monitor) flush(shutdown bool) {
	previous := m.flushStatus.Swap(flushInProgress)
	drained := m.client.Flush(2 * time.Second)
	status := flushTimedOut
	if drained {
		status = flushDrained
	}
	m.flushStatus.Store(status)
	if status == flushTimedOut && (shutdown || previous != flushTimedOut) {
		fault := supportreport.ProjectFault(context.Background(), "paperboat-server", "service_watchdog", "service_error", "sentry_delivery_failed", context.DeadlineExceeded)
		m.logger.Log(context.Background(), slog.LevelWarn, "Sentry job monitor flush timed out", append(fault.LogAttrs(), "flush_status", "timed_out", "sdk_http_failures", m.httpFailures.Load(), "sdk_submissions_dropped", m.submissionsDropped.Load(), "delivery", "unconfirmed")...)
	}
}

func (m *Monitor) observeDeliveryFailure(status int, err error) {
	if m == nil {
		return
	}
	m.httpFailures.Add(1)
	cause := err
	if status >= http.StatusBadRequest && status <= 599 {
		cause = supportreport.WithDiagnosticStatus(status, err)
	}
	fault := supportreport.ProjectFault(context.Background(), "paperboat-server", "service_watchdog", "service_error", "sentry_delivery_failed", cause)
	class := strings.Join([]string{fault.Cause, fault.ErrorType, strconv.Itoa(fault.Errno), strconv.Itoa(fault.HTTPStatus)}, "\x00")
	m.deliveryMu.Lock()
	changed := !m.deliveryFailed || class != m.deliveryClass
	m.deliveryFailed, m.deliveryClass = true, class
	m.deliveryMu.Unlock()
	if changed {
		m.logger.Log(context.Background(), faultLevel(fault), "Sentry job monitor delivery failed", fault.LogAttrs()...)
	}
}

func faultLevel(fault supportreport.Fault) slog.Level {
	switch fault.Severity {
	case "info":
		return slog.LevelInfo
	case "warning":
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

func (m *Monitor) logDeliveryFailure(err error) {
	fault := supportreport.ProjectFault(context.Background(), "paperboat-server", "service_watchdog", "service_error", "sentry_delivery_failed", err)
	m.logger.Log(context.Background(), slog.LevelError, "Sentry job monitor submission failed", fault.LogAttrs()...)
}

func (m *Monitor) observeDeliverySuccess() {
	m.deliveryMu.Lock()
	wasFailed := m.deliveryFailed
	m.deliveryFailed, m.deliveryClass = false, ""
	m.deliveryMu.Unlock()
	if wasFailed {
		fault := supportreport.ProjectFault(context.Background(), "paperboat-server", "service_watchdog", "service_error", "worker_recovered", nil)
		m.logger.Log(context.Background(), slog.LevelInfo, "Sentry job monitor delivery recovered", fault.LogAttrs()...)
	}
}
