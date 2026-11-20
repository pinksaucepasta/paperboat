package diagnostics

import (
	"context"
	"sync"
	"time"
)

type stagesKey struct{}

// Stages times the consecutive steps of one request for its access-log line: each Stage call records the time since the
// previous call (the first: since the request started) as <name>_ms. It holds step names and durations only.
type Stages struct {
	mu    sync.Mutex
	last  time.Time
	attrs []any
}

func WithStages(ctx context.Context) (context.Context, *Stages) {
	s := &Stages{last: time.Now()}
	return context.WithValue(ctx, stagesKey{}, s), s
}

// Stage records the step that just finished; a context without Stages (a unit test, a background caller) ignores it.
func Stage(ctx context.Context, name string) {
	s, ok := ctx.Value(stagesKey{}).(*Stages)
	if !ok {
		return
	}
	now := time.Now()
	s.mu.Lock()
	s.attrs = append(s.attrs, name+"_ms", now.Sub(s.last).Milliseconds())
	s.last = now
	s.mu.Unlock()
}

// Attrs are the recorded steps as slog key/value pairs.
func (s *Stages) Attrs() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.attrs...)
}
