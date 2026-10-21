package tunnelmanager

import (
	"context"
	"crypto/x509"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/originhealth"
)

type switchableProber struct {
	mu  sync.Mutex
	err error
}

func (p *switchableProber) set(err error) { p.mu.Lock(); p.err = err; p.mu.Unlock() }
func (p *switchableProber) ProbeOrigin(context.Context, hoststate.TunnelConfigRoute) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

type recordingReporter struct {
	reports chan OriginHealthReport
}

func (r *recordingReporter) ReportOriginHealth(_ context.Context, report OriginHealthReport) error {
	r.reports <- report
	return nil
}

func nextReport(t *testing.T, reporter *recordingReporter) OriginHealthReport {
	t.Helper()
	select {
	case report := <-reporter.reports:
		return report
	case <-time.After(3 * time.Second):
		t.Fatal("no origin health report")
		return OriginHealthReport{}
	}
}

// A stopped local app is reported as unreachable (with why) after consecutive failed probes and
// as ready again when it returns; the first success is not reported.
func TestOriginHealthReportsAStoppedAndRestartedApp(t *testing.T) {
	prober := &switchableProber{}
	reporter := &recordingReporter{reports: make(chan OriginHealthReport, 8)}
	identity := originHealthIdentity{tunnelID: "tun_1", connectorID: "conn_1", sessionID: "session_1", processGeneration: 3, generation: 5, contentHash: "sha256:" + string(make([]byte, 0))}
	routes := []hoststate.TunnelConfigRoute{{ID: "route_1", DesiredState: "active"}, {ID: "route_off", DesiredState: "paused"}}
	stop := startOriginHealth(t.Context(), identity, routes, prober, reporter, 10*time.Millisecond)
	defer stop()

	time.Sleep(60 * time.Millisecond)
	select {
	case report := <-reporter.reports:
		t.Fatalf("a healthy app was reported: %+v", report)
	default:
	}

	prober.set(&originProbeError{syscall.ECONNREFUSED})
	down := nextReport(t, reporter)
	if len(down.Routes) != 1 || down.Routes[0].RouteID != "route_1" || down.Routes[0].State != "unreachable" || down.Routes[0].Code != "refused" {
		t.Fatalf("down report %+v", down)
	}
	if down.TunnelID != "tun_1" || down.ConnectorID != "conn_1" || down.SessionID != "session_1" || down.ProcessGeneration != 3 || down.ConfigGeneration != 5 {
		t.Fatalf("report not bound to the session: %+v", down)
	}

	prober.set(nil)
	up := nextReport(t, reporter)
	if up.Routes[0].State != "ready" || up.Routes[0].Code != "" {
		t.Fatalf("recovery report %+v", up)
	}
}

type originProbeError struct{ error }

func (e *originProbeError) Unwrap() error { return e.error }

func TestOriginHealthCodeNamesTheFailure(t *testing.T) {
	cases := []struct {
		reason originhealth.Reason
		err    error
		want   string
	}{
		{originhealth.ReasonRefused, syscall.ECONNREFUSED, "refused"},
		{originhealth.ReasonTimeout, context.DeadlineExceeded, "timeout"},
		{originhealth.ReasonUnreachable, syscall.EHOSTUNREACH, "unreachable"},
		{originhealth.ReasonOther, errors.New("boom"), "unreachable"},
		{originhealth.ReasonOther, x509.UnknownAuthorityError{}, "tls"},
	}
	for _, tc := range cases {
		if got := originHealthCode(tc.reason, tc.err); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.err, got, tc.want)
		}
	}
}
