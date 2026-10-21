package tunnelmanager

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/originhealth"
)

// OriginHealthRoute is the latest reachability of one route's local app. Code is set only for
// unreachable: refused, timeout, unreachable or tls.
type OriginHealthRoute struct {
	RouteID    string
	State      string
	Code       string
	ObservedAt time.Time
}

// OriginHealthReport carries route reachability for one authenticated connector session; the
// control plane binds it to that exact session and configuration.
type OriginHealthReport struct {
	TunnelID          string
	ConnectorID       string
	SessionID         string
	ProcessGeneration uint64
	ConfigGeneration  uint64
	ContentHash       string
	Routes            []OriginHealthRoute
}

// OriginHealthReporter delivers an OriginHealthReport to the control plane.
type OriginHealthReporter interface {
	ReportOriginHealth(context.Context, OriginHealthReport) error
}

const originHealthReportTimeout = 20 * time.Second

// originHealthIdentity is the exact session a generation's reports are bound to.
type originHealthIdentity struct {
	tunnelID, connectorID, sessionID string
	processGeneration, generation    uint64
	contentHash                      string
}

// startOriginHealth probes every active route's local app and reports changes (and a repeat while
// it stays down) so `pb tunnel status` can say the app stopped. It stops when the returned
// function is called or ctx ends. Reports are best effort: the control plane ages an unreachable
// report out, so a lost one is repaired by the next repeat or by aging.
func startOriginHealth(ctx context.Context, identity originHealthIdentity, routes []hoststate.TunnelConfigRoute, prober OriginProber, reporter OriginHealthReporter, interval time.Duration) func() {
	ctx, cancel := context.WithCancel(ctx)
	events := make(chan originhealth.Event, 64)
	var lastError sync.Map // route ID -> last probe error, to tell a TLS failure from a refused dial
	tracker := originhealth.New(func(event originhealth.Event) {
		select {
		case events <- event:
		default:
		}
	}, 0, nil)
	var group sync.WaitGroup
	for _, route := range routes {
		if route.DesiredState != "active" || route.ID == "" {
			continue
		}
		group.Add(1)
		go func() {
			defer group.Done()
			tracker.Probe(ctx, route.ID, interval, func(probeCtx context.Context) error {
				err := prober.ProbeOrigin(probeCtx, route)
				lastError.Store(route.ID, err)
				return err
			})
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		reportedDown := map[string]bool{}
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-events:
				up := event.State == originhealth.Up
				if up && !reportedDown[event.Key] {
					continue // the first success is not news; only a recovery is
				}
				route := OriginHealthRoute{RouteID: event.Key, State: "ready", ObservedAt: event.At.UTC()}
				if !up {
					value, _ := lastError.Load(event.Key)
					err, _ := value.(error)
					route.State, route.Code = "unreachable", originHealthCode(event.Reason, err)
				}
				sendCtx, done := context.WithTimeout(ctx, originHealthReportTimeout)
				err := reporter.ReportOriginHealth(sendCtx, OriginHealthReport{
					TunnelID: identity.tunnelID, ConnectorID: identity.connectorID, SessionID: identity.sessionID,
					ProcessGeneration: identity.processGeneration, ConfigGeneration: identity.generation,
					ContentHash: identity.contentHash, Routes: []OriginHealthRoute{route},
				})
				done()
				if err == nil {
					reportedDown[event.Key] = !up
				}
			}
		}
	}()
	return func() {
		cancel()
		group.Wait()
	}
}

// originHealthCode maps a probe failure to the finite code the control plane accepts.
func originHealthCode(reason originhealth.Reason, err error) string {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &verification) || errors.As(err, &record) {
		return "tls"
	}
	switch reason {
	case originhealth.ReasonRefused:
		return "refused"
	case originhealth.ReasonTimeout:
		return "timeout"
	default:
		return "unreachable"
	}
}
