package edgehttp

import (
	"context"
	"errors"
	"io"
	"sync"
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

// meterRuntimeStream records a runtime session's bytes under its authority and ends the
// stream when the server reports that authority's workspace out of allowance.
func meterRuntimeStream(usage IngressUsage, binding runtimeUsageBinding, stream io.ReadWriteCloser) io.ReadWriteCloser {
	s := &runtimeUsageStream{stream: stream, usage: usage, binding: binding}
	s.unsubscribe = usage.SubscribeQuota(binding.Authority, binding.IssuedAt, func() {
		s.mu.Lock()
		s.exhausted = true
		s.mu.Unlock()
		_ = stream.Close()
	})
	return s
}

type runtimeUsageStream struct {
	stream      io.ReadWriteCloser
	usage       IngressUsage
	binding     runtimeUsageBinding
	unsubscribe func()
	mu          sync.Mutex
	exhausted   bool
	closeOnce   sync.Once
}

func (s *runtimeUsageStream) quotaExhausted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exhausted
}

func (s *runtimeUsageStream) Read(payload []byte) (int, error) {
	if s.quotaExhausted() {
		return 0, ErrIngressQuotaExhausted
	}
	n, err := s.stream.Read(payload)
	if n > 0 {
		err = errors.Join(err, s.usage.Record(s.binding.Environment, s.binding.Route, s.binding.Revision, 0, uint64(n), s.binding.Authority))
	}
	if err != nil && s.quotaExhausted() {
		return n, ErrIngressQuotaExhausted
	}
	return n, err
}

func (s *runtimeUsageStream) Write(payload []byte) (int, error) {
	if s.quotaExhausted() {
		return 0, ErrIngressQuotaExhausted
	}
	n, err := s.stream.Write(payload)
	if n > 0 {
		err = errors.Join(err, s.usage.Record(s.binding.Environment, s.binding.Route, s.binding.Revision, uint64(n), 0, s.binding.Authority))
	}
	if err != nil && s.quotaExhausted() {
		return n, ErrIngressQuotaExhausted
	}
	return n, err
}

func (s *runtimeUsageStream) CloseWrite() error {
	if closer, ok := s.stream.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

func (s *runtimeUsageStream) Close() error {
	s.closeOnce.Do(func() {
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
	})
	return s.stream.Close()
}
