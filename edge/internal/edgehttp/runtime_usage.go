package edgehttp

import (
	"context"
	"errors"
	"io"
	"time"
)

// runtimeUsageBinding is the usage authority of one edge runtime session (exec, ssh,
// terminal, file transfer through runtime_https_wss): the verified runtime credential's
// assignment_id, which the server recorded as the session's accounting authority when it
// issued the descriptor, and the runtime route it was issued for.
type runtimeUsageBinding struct {
	Authority   string
	Environment string
	Route       string
	Revision    uint64
	IssuedAt    time.Time
}

type runtimeUsageKey struct{}

func withRuntimeUsage(ctx context.Context, binding runtimeUsageBinding) context.Context {
	return context.WithValue(ctx, runtimeUsageKey{}, binding)
}

func runtimeUsageFrom(ctx context.Context) (runtimeUsageBinding, bool) {
	binding, ok := ctx.Value(runtimeUsageKey{}).(runtimeUsageBinding)
	return binding, ok && binding.Authority != "" && binding.Environment != ""
}

// meterRuntimeStream records a runtime session's bytes under its authority.
func meterRuntimeStream(usage IngressUsage, binding runtimeUsageBinding, stream io.ReadWriteCloser) io.ReadWriteCloser {
	return &runtimeUsageStream{stream: stream, usage: usage, binding: binding}
}

type runtimeUsageStream struct {
	stream  io.ReadWriteCloser
	usage   IngressUsage
	binding runtimeUsageBinding
}

func (s *runtimeUsageStream) Read(payload []byte) (int, error) {
	n, err := s.stream.Read(payload)
	if n > 0 {
		err = errors.Join(err, s.usage.Record(s.binding.Environment, s.binding.Route, s.binding.Revision, 0, uint64(n), s.binding.Authority))
	}
	return n, err
}

func (s *runtimeUsageStream) Write(payload []byte) (int, error) {
	n, err := s.stream.Write(payload)
	if n > 0 {
		err = errors.Join(err, s.usage.Record(s.binding.Environment, s.binding.Route, s.binding.Revision, uint64(n), 0, s.binding.Authority))
	}
	return n, err
}

func (s *runtimeUsageStream) CloseWrite() error {
	if closer, ok := s.stream.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

func (s *runtimeUsageStream) Close() error { return s.stream.Close() }
