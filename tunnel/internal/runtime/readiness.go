package runtime

import (
	"context"
	"errors"
	"time"
)

var ErrReadinessTimeout = errors.New("dependency readiness timed out")

type Readiness struct {
	Probe    func() error
	Timeout  time.Duration
	Interval time.Duration
}

func (r Readiness) Start(ctx context.Context) error {
	if ctx == nil || r.Probe == nil || r.Timeout <= 0 || r.Interval <= 0 {
		return ErrProcessInvalid
	}
	deadline := time.NewTimer(r.Timeout)
	defer deadline.Stop()
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = r.Probe()
		if lastErr == nil {
			return nil
		}
		timer := time.NewTimer(r.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-deadline.C:
			timer.Stop()
			return errors.Join(ErrReadinessTimeout, context.DeadlineExceeded, lastErr)
		case <-timer.C:
		}
	}
}

func (Readiness) Shutdown(context.Context) error { return nil }
