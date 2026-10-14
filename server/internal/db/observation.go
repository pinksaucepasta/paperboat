package db

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// QueryObserver gets no SQL or parameters. Its finish callback receives only a
// coarse outcome. Install once during composition; replacement is race-safe.
type QueryObserver func(context.Context) (context.Context, func(string))
type queryObserver struct{ observer atomic.Pointer[QueryObserver] }
type queryObservationKey struct{}

func (d *DB) SetQueryObserver(observer QueryObserver) {
	if d != nil && d.observer != nil {
		d.observer.observer.Store(&observer)
	}
}
func (o *queryObserver) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	fn := o.observer.Load()
	if fn == nil || *fn == nil {
		return ctx
	}
	next, finish := (*fn)(ctx)
	return context.WithValue(next, queryObservationKey{}, finish)
}
func (o *queryObserver) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	finish, _ := ctx.Value(queryObservationKey{}).(func(string))
	if finish == nil {
		return
	}
	outcome := "success"
	if data.Err != nil {
		outcome = "failed"
		if queryCanceled(data.Err) {
			outcome = "canceled"
		}
	}
	finish(outcome)
}

func queryCanceled(err error) (canceled bool) {
	defer func() {
		if recover() != nil {
			canceled = false
		}
	}()
	for depth := 0; err != nil && depth < 8; depth++ {
		if err == context.Canceled {
			return true
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}
