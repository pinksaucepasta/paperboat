package diagnostics

import (
	"context"
	"crypto/rand"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

const ReconnectHeader = "X-Paperboat-Reconnect"

type reconnectKey struct{}
type reconnectTrace struct{ attempt, caller, connection string }

// WithReconnect correlates only locally generated identifiers, never resource IDs or content.
func WithReconnect(ctx context.Context, attempt, caller, connection string) context.Context {
	if !safeIdentifier(attempt, 64) || !safeIdentifier(connection, 64) || strings.ContainsAny(attempt+connection, ":") || (caller != "supervise" && caller != "probeStalled") {
		return ctx
	}
	return context.WithValue(ctx, reconnectKey{}, reconnectTrace{attempt, caller, connection})
}
func ReconnectHeaders(ctx context.Context, header http.Header) {
	if trace, ok := ctx.Value(reconnectKey{}).(reconnectTrace); ok {
		header.Set(ReconnectHeader, trace.attempt+":"+trace.caller+":"+trace.connection)
	}
}
func ReconnectRequest(ctx context.Context, header http.Header) context.Context {
	value := header.Get(ReconnectHeader)
	// Identifiers contain no colons; reject malformed local IPC metadata.
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return ctx
	}
	return WithReconnect(ctx, parts[0], parts[1], parts[2])
}

// ReconnectEvent uses the existing bounded recorder and typed fault projection only.
func ReconnectEvent(ctx context.Context, code, state string, err error) {
	trace, ok := ctx.Value(reconnectKey{}).(reconnectTrace)
	if !ok {
		return
	}
	fields := map[string]string{"attempt_id": trace.attempt, "caller": trace.caller, "connection_id": trace.connection, "state": state}
	recordReconnect(ctx, code, fields, err, 2)
}

// ReconnectStep marks the actual call site. Matching step IDs remain unambiguous across processes.
func ReconnectStep(ctx context.Context, step string) func(error) {
	trace, ok := ctx.Value(reconnectKey{}).(reconnectTrace)
	if !ok {
		return func(error) {}
	}
	fields := map[string]string{"attempt_id": trace.attempt, "caller": trace.caller, "connection_id": trace.connection, "step": step, "step_id": rand.Text(), "state": "started"}
	started := time.Now()
	recordReconnect(ctx, "redial_step", fields, nil, 2)
	return func(err error) {
		fields["state"] = "ready"
		if err != nil {
			fields["state"] = "failed"
		}
		fields["duration_ms"] = strconv.FormatInt(time.Since(started).Milliseconds(), 10)
		recordReconnect(ctx, "redial_step", fields, err, 2)
	}
}
func recordReconnect(ctx context.Context, code string, fields map[string]string, err error, skip int) {
	recorder := FromContext(ctx)
	if recorder == nil {
		return
	}
	if _, file, line, ok := runtime.Caller(skip); ok {
		fields["source_file"] = filepath.Base(file)
		fields["source_line"] = strconv.Itoa(line)
	}
	if err != nil {
		fault := errorreport.ProjectFault(ctx, "pb", "connect", "peer_connect", "terminal_reconnect_failed", err)
		fields["cause"], fields["error_type"] = fault.Cause, fault.ErrorType
		fields["outcome"] = fault.Outcome
		if len(fault.ErrorChain) > 0 {
			fields["error_chain"] = strings.Join(fault.ErrorChain, ":")
		}
		if fault.HTTPStatus != 0 {
			fields["http_status"] = strconv.Itoa(fault.HTTPStatus)
		}
	}
	_ = recorder.RecordWithSupportReference("terminal", code, "info", supportref.FromContext(ctx), fields)
}
