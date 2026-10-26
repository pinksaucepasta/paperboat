package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// boundedAcquire is the request pool's tracer: the query observer, plus a bound on how long one Acquire waits
// for a connection. pgxpool waits for a free connection until the caller's context ends; with a full pool every
// request would wait for its whole request deadline. The bound covers the wait alone, never the query that runs
// on the connection, and ends as context.DeadlineExceeded, which the HTTP layer answers with 503 + Retry-After.
type boundedAcquire struct {
	*queryObserver
	timeout time.Duration
}

type acquireCancelKey struct{}

func (b boundedAcquire) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	bounded, cancel := context.WithTimeout(ctx, b.timeout)
	return context.WithValue(bounded, acquireCancelKey{}, cancel)
}

func (b boundedAcquire) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireEndData) {
	if cancel, ok := ctx.Value(acquireCancelKey{}).(context.CancelFunc); ok {
		cancel()
	}
}
