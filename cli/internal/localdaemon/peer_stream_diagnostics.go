package localdaemon

import (
	"context"
	"io"
	"net"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

func reportLocalPeerBridgeFailure(ctx context.Context, err error) {
	if err == nil || normalLocalPeerTermination(err) {
		return
	}
	errorreport.Current().CaptureFailure(ctx, "paperboatd", "peer_stream", "delivery", "terminal_session_failed", err)
}

// A joined cancellation cannot hide an independent stream failure. Unresolved
// wrappers, nil children and cycles fail closed within this finite traversal.
func normalLocalPeerTermination(err error) bool {
	pending := []error{err}
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if value.Type().Comparable() && (current == context.Canceled || current == io.EOF || current == io.ErrClosedPipe || current == net.ErrClosed) {
			continue
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 15-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		default:
			return false
		}
	}
	return true
}
