package runtime

import (
	"context"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
)

// HTTP attempts already publish through the control transport. This boundary
// owns failures while decoding or applying verified authority locally.
func observeWorkerFailure(ctx context.Context, reporter *reporting.Reporter, definition string, err error) {
	if err == nil || reporter == nil {
		return
	}
	if httpOwnedFailure(err) {
		return
	}
	reporter.ObserveFailure(ctx, definition, err)
}

// A joined local mutation failure and a failed ACK must still publish the
// mutation cause. Only a sole request failure has already been observed.
func httpOwnedFailure(err error) bool {
	for depth := 0; err != nil && depth < 8; depth++ {
		if request, ok := err.(*control.RequestFailure); ok {
			return request != nil
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}
