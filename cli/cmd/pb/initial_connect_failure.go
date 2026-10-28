package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

// retryableInitialConnectFailure requires every independent branch to be a
// known transient failure. Transport markers never authorize hiding a sibling.
func retryableInitialConnectFailure(err error) (retryable bool) {
	defer func() {
		if recover() != nil {
			retryable = false
		}
	}()
	remaining := 32
	var visit func(error, int) bool
	visit = func(current error, status int) bool {
		if current == nil || remaining == 0 {
			return false
		}
		remaining--
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
			if value.IsNil() {
				return false
			}
		}
		if owner, ok := current.(*localapi.RemoteError); ok {
			// These codes name transient native transport or terminal-service failures.
			// Other 503s remain authoritative.
			if owner.StatusCode != http.StatusServiceUnavailable || (owner.Code != "peer_transport_unavailable" && owner.Code != "terminal_service_unavailable") {
				return false
			}
			return owner.Unwrap() == nil || visit(owner.Unwrap(), owner.StatusCode)
		}
		if owner, ok := current.(*api.APIError); ok {
			if (owner.Code != "machine_not_ready" && owner.Code != "tunnel_unavailable") ||
				!(owner.Status == 0 || owner.Status == 409 || owner.Status >= 500 && owner.Status <= 599) {
				return false
			}
			return owner.Unwrap() == nil || visit(owner.Unwrap(), owner.Status)
		}
		// A lookup that timed out on a lossy link is transient; a name that does not exist is not.
		if dns, ok := current.(*net.DNSError); ok {
			return dns.IsTimeout || dns.IsTemporary
		}
		switch current {
		case context.Canceled, context.DeadlineExceeded:
			return false
		case tunnel.ErrPeerStreamOpen, tunnel.ErrTransportLost, localapi.ErrTransportUnavailable,
			syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ETIMEDOUT,
			syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.EPIPE,
			io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, net.ErrClosed, os.ErrDeadlineExceeded:
			return true
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > remaining {
				return false
			}
			for _, child := range children {
				if !visit(child, status) {
					return false
				}
			}
			return true
		}
		if wrapper, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapper.Unwrap(), status)
		}
		// The API's private cause preserves a body-free HTTP status through
		// the owned transport marker. Only matching terminal status metadata
		// can complete this proof; wrappers and siblings were checked above.
		if metadata, ok := current.(interface{ DiagnosticStatus() int }); ok {
			return status != 0 && metadata.DiagnosticStatus() == status
		}
		return false
	}
	return visit(err, 0)
}
