package reattach

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRecoverySharesBudgetAcrossLostConnectionsAndStopsOnDenial(t *testing.T) {
	transient := errors.New("connection lost")
	denied := errors.New("access revoked")
	for _, permanent := range []bool{false, true} {
		r := New(Policy{Attempts: 2, Retryable: func(err error) bool { return errors.Is(err, transient) }})
		calls := 0
		err := r.Run(t.Context(), func(context.Context) error {
			calls++
			if permanent {
				return denied
			}
			if calls == 1 {
				return transient
			}
			return nil
		})
		if permanent {
			if !errors.Is(err, denied) || calls != 1 || r.Remaining() != 1 {
				t.Fatal("denial was retried", err, calls)
			}
		} else {
			if err != nil || calls != 2 || r.Remaining() != 0 {
				t.Fatal("transient recovery abandoned budget", err, calls)
			}
			if err := r.Run(t.Context(), func(context.Context) error { t.Fatal("replenished exhausted budget"); return nil }); !errors.Is(err, ErrExhausted) {
				t.Fatal(err)
			}
		}
	}
}
func TestCanceledRecoveryDoesNotAttemptDial(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := New(Policy{Attempts: 2, Backoff: func(int) time.Duration { return time.Second }})
	if err := r.Run(ctx, func(context.Context) error { t.Fatal("dialed after cancellation"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
