package connectorprotocol

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"reflect"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

const ControlWebSocketSubprotocol = "paperboat.connector.v1"

// WebSocketHandler adapts HTTPS upgrade requests to the byte-stream control
// transport. Connector-v1 authentication remains inside the signed Hello; no
// browser origin, cookie, query credential, or bearer fallback is accepted.
type WebSocketHandler struct {
	transport *ControlTransport
	gone      ConnectorGoneFunc
}

// ConnectorGoneFunc reports whether the connector's tunnel was deleted or the connector revoked.
type ConnectorGoneFunc func(ctx context.Context, tunnelID, connectorID string) (bool, error)

// WithGoneCheck makes the handler refuse a connector that can never be served again with 410 before
// the upgrade, so a host stops reconnecting instead of retrying a rejected stream forever.
func (h *WebSocketHandler) WithGoneCheck(gone ConnectorGoneFunc) *WebSocketHandler {
	h.gone = gone
	return h
}

// SQLConnectorGone answers ConnectorGoneFunc from the database. A connector is gone when it is
// unknown or revoked, its tunnel was deleted or has expired, or its host machine was revoked or
// deleted: none of these can be served again, so the host must stop retrying.
func SQLConnectorGone(database *db.DB) ConnectorGoneFunc {
	return func(ctx context.Context, tunnelID, connectorID string) (bool, error) {
		var served bool
		err := database.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tunnel_connectors c JOIN tunnels t ON t.id = c.tunnel_id
 JOIN user_machines m ON m.id = c.host_id
 WHERE t.id = $1 AND c.id = $2 AND t.deleted_at IS NULL AND t.desired_state <> 'deleted'
 AND (t.expires_at IS NULL OR t.expires_at > now())
 AND c.revoked_at IS NULL AND c.desired_state <> 'revoked'
 AND m.revoked_at IS NULL AND m.deleted_at IS NULL)`, tunnelID, connectorID).Scan(&served)
		return !served, err
	}
}

func NewWebSocketHandler(transport *ControlTransport) (*WebSocketHandler, error) {
	if transport == nil {
		return nil, wire.ErrInvalidInput
	}
	return &WebSocketHandler{transport: transport}, nil
}

func (h *WebSocketHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.transport == nil || r.Method != http.MethodGet {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	tunnelID, connectorID := r.PathValue("tunnel_id"), r.PathValue("connector_id")
	if wire.ValidateIdentifier(tunnelID) != nil || wire.ValidateIdentifier(connectorID) != nil || r.URL.RawQuery != "" {
		http.Error(w, "invalid connector control request", http.StatusBadRequest)
		return
	}
	if h.gone != nil {
		if gone, err := h.gone(r.Context(), tunnelID, connectorID); err == nil && gone {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"code":"connector_gone","message":"The tunnel was deleted or the connector was revoked; remove it from this machine.","retryable":false}`))
			return
		}
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{ControlWebSocketSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	if connection.Subprotocol() != ControlWebSocketSubprotocol {
		_ = connection.Close(websocket.StatusPolicyViolation, "connector subprotocol required")
		return
	}
	connection.SetReadLimit(wire.MaxFrameBytes + 4)
	stream := websocket.NetConn(r.Context(), connection, websocket.MessageBinary)
	if err = h.transport.Serve(r.Context(), stream, tunnelID, connectorID); err != nil {
		requestedCode, expectedRejection, canceled := classifyWebSocketFailure(err)
		if requestedCode == "" {
			requestedCode = "connector_control_failed"
		}
		fault := supportreport.ProjectFault(r.Context(), "paperboat-server", "connector_connection", "http_handler", requestedCode, err)
		if fault.Code != requestedCode {
			requestedCode = "connector_control_failed"
			fault = supportreport.ProjectFault(r.Context(), "paperboat-server", "connector_connection", "http_handler", requestedCode, err)
		}
		if canceled {
			fault.Outcome, fault.Severity = "canceled", "info"
			slog.InfoContext(r.Context(), "connector control stream canceled", fault.LogAttrs()...)
		} else if expectedRejection && fault.Code == requestedCode {
			fault.Outcome, fault.Severity = "rejected", "warning"
			slog.WarnContext(r.Context(), "connector control stream rejected", fault.LogAttrs()...)
		} else {
			supportreport.CaptureFromContext(r.Context(), "paperboat-server", "connector_connection", "http_handler", requestedCode, err)
		}
		_ = connection.Close(websocket.StatusPolicyViolation, "connector control rejected")
		return
	}
	_ = connection.Close(websocket.StatusNormalClosure, "connector control closed")
}

const websocketErrorWalkLimit = 32

// classifyWebSocketFailure treats a protocol code as an expected rejection
// only when every leaf in the bounded error tree is a known protocol error or
// an exact request cancellation. A code wrapper cannot hide an independent
// provider, persistence, or transport failure.
func classifyWebSocketFailure(err error) (code string, expectedRejection, canceled bool) {
	if err == nil {
		return "", false, false
	}
	type errorFrame struct {
		err               error
		malformedFrameEOF bool
	}
	pending := []errorFrame{{err: err}}
	nodes, sawTerminal, allCanceled := 0, false, true
	allExpected := true
	for len(pending) > 0 {
		if nodes >= websocketErrorWalkLimit {
			return code, false, false
		}
		last := len(pending) - 1
		frame := pending[last]
		current := frame.err
		pending = pending[:last]
		if nilErrorValue(current) {
			return code, false, false
		}
		nodes++
		if typed, ok := current.(*wire.Error); ok {
			if typed == nil || !knownProtocolCode(typed.Code) {
				return code, false, false
			}
			if code == "" {
				code = string(typed.Code)
			}
			if !expectedProtocolRejectionCode(typed.Code) {
				allExpected = false
			}
			if typed.Cause == nil {
				sawTerminal = true
				if typed.Code != wire.CodeCanceled {
					allCanceled = false
				}
			} else {
				if nodes+len(pending) >= websocketErrorWalkLimit {
					return code, false, false
				}
				pending = append(pending, errorFrame{err: typed.Cause, malformedFrameEOF: typed.Code == wire.CodeMalformedFrame})
			}
			continue
		}
		if exactWebSocketCancellation(current) {
			sawTerminal = true
			continue
		}
		if decoded, ok := current.(*wire.ClientFrameDecodeFailure); ok && frame.malformedFrameEOF && decoded.Unwrap() != nil {
			sawTerminal = true
			allCanceled = false
			continue
		}
		if frame.malformedFrameEOF && current == io.ErrUnexpectedEOF {
			sawTerminal = true
			allCanceled = false
			continue
		}
		if expectedProtocolSentinel(current) {
			sawTerminal = true
			allCanceled = false
			continue
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || nodes+len(pending)+len(children) > websocketErrorWalkLimit {
				return code, false, false
			}
			for _, child := range children {
				pending = append(pending, errorFrame{err: child, malformedFrameEOF: frame.malformedFrameEOF})
			}
			continue
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			child := wrapped.Unwrap()
			if nilErrorValue(child) || nodes+len(pending) >= websocketErrorWalkLimit {
				return code, false, false
			}
			pending = append(pending, errorFrame{err: child, malformedFrameEOF: frame.malformedFrameEOF})
			continue
		}
		return code, false, false
	}
	if !sawTerminal {
		return code, false, false
	}
	if allCanceled {
		return code, false, true
	}
	return code, code != "" && allExpected, false
}

func expectedProtocolSentinel(err error) bool {
	value := reflect.ValueOf(err)
	if !value.IsValid() || !value.Comparable() {
		return false
	}
	for _, expected := range []error{
		ErrDurableReplay,
		ErrConnectorNotFound,
		ErrStaleProcess,
		ErrSessionNotFound,
		ErrConfigNotFound,
		ErrOperationNotFound,
	} {
		if err == expected {
			return true
		}
	}
	return false
}

func knownProtocolCode(code wire.Code) bool {
	switch code {
	case wire.CodeInvalidInput, wire.CodeMalformedFrame, wire.CodeFrameTooLarge, wire.CodeProtocolIncompatible,
		wire.CodeCapabilityMissing, wire.CodeAuthenticationFailed, wire.CodeCredentialExpired, wire.CodeIdentityMismatch,
		wire.CodeSessionConflict, wire.CodeSessionClosed, wire.CodeLeaseExpired, wire.CodeHeartbeatTimeout,
		wire.CodeSnapshotRequired, wire.CodeGenerationGap, wire.CodeStaleGeneration, wire.CodeContentHashMismatch,
		wire.CodeSnapshotRejected, wire.CodeDeltaRejected, wire.CodeNotReady, wire.CodeCanceled, wire.CodeStaleSession,
		wire.CodeUnsupportedMessage, wire.CodeDrainRejected, wire.CodeDrainTimeout,
		wire.CodeCredentialRotationRejected, wire.CodeCredentialRotationNotReady, wire.CodeCredentialRotationFailed:
		return true
	default:
		return false
	}
}

func expectedProtocolRejectionCode(code wire.Code) bool {
	switch code {
	case wire.CodeInvalidInput, wire.CodeMalformedFrame, wire.CodeFrameTooLarge, wire.CodeProtocolIncompatible,
		wire.CodeCapabilityMissing, wire.CodeAuthenticationFailed, wire.CodeCredentialExpired, wire.CodeIdentityMismatch,
		wire.CodeSessionConflict, wire.CodeSessionClosed, wire.CodeLeaseExpired, wire.CodeHeartbeatTimeout,
		wire.CodeSnapshotRequired, wire.CodeGenerationGap, wire.CodeStaleGeneration, wire.CodeContentHashMismatch,
		wire.CodeSnapshotRejected, wire.CodeDeltaRejected, wire.CodeNotReady, wire.CodeCanceled, wire.CodeStaleSession,
		wire.CodeUnsupportedMessage, wire.CodeDrainRejected, wire.CodeCredentialRotationRejected,
		wire.CodeCredentialRotationNotReady:
		return true
	default:
		return false
	}
}

func nilErrorValue(err error) bool {
	if err == nil {
		return true
	}
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func exactWebSocketCancellation(err error) bool {
	value, target := reflect.ValueOf(err), reflect.ValueOf(context.Canceled)
	return value.IsValid() && target.IsValid() && value.Type() == target.Type() && value.Pointer() == target.Pointer()
}
