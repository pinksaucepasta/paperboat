package tunnelmanager

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

type ingressBindingKey struct{}

// ingressExpiryKey carries the current ingress decision expiry alongside the
// binding. The inspector replay registry uses it to fence deliberate replay
// without trusting retained bytes.
type ingressExpiryKey struct{}

func (f OriginStreamForwarder) admitIngress(parent context.Context, stream io.ReadWriteCloser, open connectorprotocol.StreamOpen, route hoststate.TunnelConfigRoute) (context.Context, context.CancelFunc, error) {
	ctx, cancelCause := context.WithCancelCause(parent)
	cancel := func() { cancelCause(nil) }
	fail := func(err error) (context.Context, context.CancelFunc, error) { cancel(); return nil, nil, err }
	// A peer cannot hold origin admission indefinitely with a partial preface.
	stopRead := time.AfterFunc(10*time.Second, func() { _ = stream.Close() })
	decision, err := connectorprotocol.ReadIngressDecision(stream, time.Now().UTC())
	stopRead.Stop()
	if err != nil {
		originDiagnosticLogger.WarnContext(ctx, "durable ingress rejected", "stage", "decision_read")
		return fail(err)
	}
	if open.Kind == "http_browser" && decision.Binding.Audience == "public" {
		originDiagnosticLogger.WarnContext(ctx, "durable ingress rejected", "stage", "audience")
		return fail(connectorprotocol.ErrIngressDenied)
	}
	if carrierStream, ok := stream.(*connector.DataCarrierStream); ok {
		target := carrierStream.EdgeTarget()
		if target.EdgeID != decision.EdgeNodeID || target.ProcessEpoch != decision.EdgeProcessEpoch {
			originDiagnosticLogger.WarnContext(ctx, "durable ingress rejected", "stage", "edge_identity")
			return fail(connectorprotocol.ErrIngressDenied)
		}
	}
	lookup, stopLookup := context.WithDeadline(ctx, decision.ExpiresAt)
	current, err := f.IngressAuthority(lookup, open, decision)
	stopLookup()
	if err != nil {
		originDiagnosticLogger.WarnContext(ctx, "durable ingress rejected", "stage", "authority_lookup")
		captureIngressFailure(ctx, "peer_authority", err)
		return fail(ingressOperationFailure{cause: err})
	}
	if decision.Authorize(current, open, current.EdgeNodeID, current.EdgeProcessEpoch, time.Now().UTC()) != nil {
		originDiagnosticLogger.WarnContext(ctx, "durable ingress rejected", "stage", "authority_binding")
		return fail(connectorprotocol.ErrIngressDenied)
	}
	if !ingressRouteMatches(current.Binding, route) {
		originDiagnosticLogger.WarnContext(ctx, "durable ingress rejected", "stage", "origin_binding")
		return fail(connectorprotocol.ErrIngressDenied)
	}
	ctx = context.WithValue(ctx, ingressBindingKey{}, current.Binding)
	ctx = context.WithValue(ctx, ingressExpiryKey{}, current.ExpiresAt)
	terminate := ingressTermination(ctx, cancelCause, stream)
	expire := time.AfterFunc(time.Until(current.ExpiresAt), func() { terminate(nil) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer expire.Stop()
		tick := time.NewTicker(connectorprotocol.IngressRefreshInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				next, err := f.refreshIngress(ctx, open, decision, current)
				if err != nil {
					terminate(err)
					return
				}
				if ctx.Err() != nil {
					return
				}
				current = next
				expire.Reset(time.Until(next.ExpiresAt))
			}
		}
	}()
	return ctx, func() { cancel(); <-done }, nil
}

// ingressOperationFailure keeps operational causes distinct from a proven
// authorization rejection without exposing authority or origin error text.
type ingressOperationFailure struct{ cause error }

func (ingressOperationFailure) Error() string         { return "Tunnel ingress operation failed." }
func (failure ingressOperationFailure) Unwrap() error { return failure.cause }

func captureIngressFailure(ctx context.Context, stage string, err error) {
	if err == nil || ingressDenialOnly(err) {
		return
	}
	code, operation := "peer_authority_failed", "browser_authorization"
	if stage == "target_connect" {
		code, operation = "transport_failed", "origin"
	}
	errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", operation, stage, code, err)
}

// ingressTermination owns one carrier close and one final failure capture.
// Expiry can win the close race while an in-flight authority lookup still
// returns an independent error; that late cause must remain observable.
func ingressTermination(ctx context.Context, cancel context.CancelCauseFunc, stream io.Closer) func(error) error {
	var closeOnce, captureOnce sync.Once
	var closeErr error
	return func(cause error) error {
		// Revoke before closing: Close may wait on work which needs ctx canceled.
		cancel(cause)
		closeOnce.Do(func() { closeErr = stream.Close() })
		result := errors.Join(cause, closeErr)
		if result != nil && !ingressExpectedOnly(result, true) {
			faultCause := result
			if cause != nil && ingressExpectedOnly(closeErr, true) {
				// Keep expected teardown in the returned cause tree, but do not
				// let it replace the independent operational fault projection.
				faultCause = cause
			}
			captured := false
			captureOnce.Do(func() {
				captured = true
				captureIngressFailure(ctx, "peer_authority", faultCause)
			})
			if !captured && cause != nil {
				// Cleanup may already own the final exception; retain the late
				// authority cause as an observation without a second exception.
				errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "browser_authorization", "peer_authority", "peer_authority_failed", cause)
			}
		}
		return result
	}
}

func ingressDenialOnly(err error) bool { return ingressExpectedOnly(err, false) }

// Lifecycle-only quiet leaves never apply to initial lookup/origin failures.
func ingressExpectedOnly(err error, lifecycle bool) bool {
	budget := 16
	active := make(map[error]bool)
	var visit func(error) bool
	visit = func(err error) bool {
		if err == nil || budget == 0 {
			return false
		}
		budget--
		v := reflect.ValueOf(err)
		if v.Kind() == reflect.Pointer && v.IsNil() {
			return false
		}
		if v.Type().Comparable() {
			if active[err] {
				return false
			}
			active[err] = true
			defer delete(active, err)
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			if child := wrapped.Unwrap(); child != nil {
				return visit(child)
			}
		}
		return err == connectorprotocol.ErrIngressDenied || lifecycle && (err == net.ErrClosed || err == os.ErrClosed || err == context.Canceled)
	}
	return visit(err)
}

func (f OriginStreamForwarder) refreshIngress(ctx context.Context, open connectorprotocol.StreamOpen, decision, current connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
	refresh, stop := context.WithDeadline(ctx, current.ExpiresAt)
	defer stop()
	next, err := f.IngressAuthority(refresh, open, decision)
	if err != nil {
		return next, ingressOperationFailure{cause: err}
	}
	// Refresh grants time only; all independent authority dimensions remain
	// identical for the existing application stream.
	current.IssuedAt, current.ExpiresAt = next.IssuedAt, next.ExpiresAt
	if err := current.Authorize(next, open, next.EdgeNodeID, next.EdgeProcessEpoch, time.Now().UTC()); err != nil {
		return next, err
	}
	return next, nil
}

func ingressRouteMatches(b connectorprotocol.IngressBinding, r hoststate.TunnelConfigRoute) bool {
	value := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	origin := r.OriginAddress
	if host, port, err := net.SplitHostPort(origin); err == nil && strings.EqualFold(host, "localhost") {
		origin = net.JoinHostPort("127.0.0.1", port)
	}
	return b.RouteID == r.ID && b.Protocol == r.Protocol && b.OriginScheme == r.OriginScheme && b.OriginAddress == origin && b.TLSVerification == r.TLSVerification && b.TLSServerName == value(r.TLSServerName) && b.CAReference == value(r.CAReference) && b.MTLSCredentialReference == value(r.MTLSCredentialReference)
}

func validateIngressHTTPRequest(ctx context.Context, r *http.Request) error {
	b, ok := ctx.Value(ingressBindingKey{}).(connectorprotocol.IngressBinding)
	if !ok {
		return nil
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !strings.EqualFold(host, b.Hostname) || r.URL == nil || !strings.HasPrefix(r.URL.Path, b.PathPrefix) || strings.ContainsAny(r.URL.Path, "\\\x00") {
		return connectorprotocol.ErrIngressDenied
	}
	if b.PathPrefix != "/" && !strings.HasSuffix(b.PathPrefix, "/") && r.URL.Path != b.PathPrefix && !strings.HasPrefix(r.URL.Path, b.PathPrefix+"/") {
		return connectorprotocol.ErrIngressDenied
	}
	escaped := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(r.URL.Path, "/../") || strings.HasSuffix(r.URL.Path, "/..") {
		return connectorprotocol.ErrIngressDenied
	}
	return nil
}

func (f OriginStreamForwarder) serveTCP(ctx context.Context, stream io.ReadWriteCloser, route hoststate.TunnelConfigRoute) error {
	if f.IngressAuthority == nil || route.Protocol != "tcp" || route.OriginScheme != "tcp" {
		return connectorprotocol.ErrIngressDenied
	}
	dialer := f.Transport.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: time.Duration(route.ConnectTimeoutMs) * time.Millisecond}
	}
	connection, err := dialer.DialContext(ctx, "tcp", route.OriginAddress)
	if err != nil {
		captureIngressFailure(ctx, "target_connect", err)
		return ingressOperationFailure{cause: errors.Join(ErrOriginUnavailable, err)}
	}
	defer connection.Close()
	half, ok := connection.(interface{ CloseWrite() error })
	if !ok {
		return ErrInvalidConfig
	}
	halfStream, ok := stream.(interface{ CloseWrite() error })
	if !ok {
		return ErrInvalidConfig
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close(); _ = stream.Close() })
	defer stop()
	results := make(chan error, 2)
	go func() {
		_, err := io.Copy(connection, stream)
		if err == nil {
			err = half.CloseWrite()
		}
		results <- err
	}()
	go func() {
		_, err := io.Copy(stream, connection)
		if err == nil {
			err = halfStream.CloseWrite()
		}
		results <- err
	}()
	first := <-results
	if first != nil {
		_ = connection.Close()
		_ = stream.Close()
	}
	return errors.Join(first, <-results)
}

// ForwardIngressHTTP serves exactly one HTTP request over an independently
// authorized connector stream. The caller owns route concurrency and transport
// lifetime; cancellation closes the stream and any active response or upgrade.
func (f OriginStreamForwarder) ForwardIngressHTTP(ctx context.Context, stream io.ReadWriteCloser, open connectorprotocol.StreamOpen, route hoststate.TunnelConfigRoute) error {
	if f.Transport == nil || f.IngressAuthority == nil || stream == nil || ctx == nil || route.Protocol != "http" || open.Kind != "http_browser" {
		return connectorprotocol.ErrIngressDenied
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	authorized, release, err := f.admitIngress(ctx, stream, open, route)
	if err != nil {
		return err
	}
	defer release()
	return f.serveHTTP(authorized, idleOriginStream{ReadWriteCloser: stream, idle: time.Duration(route.IdleTimeoutMs) * time.Millisecond}, route, f.Transport)
}
