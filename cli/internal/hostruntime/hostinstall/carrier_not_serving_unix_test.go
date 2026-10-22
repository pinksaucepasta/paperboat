//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
)

// A retained carrier that verifies but never answers (dadape, 2026-10-22: its old pin refused its own
// trust file and exited every 5 s) used to fail preflight, so every install aborted and the carrier
// could never be replaced. "Never answered" (its readiness wait ran out with the control endpoint
// unavailable, the install still live) is now a typed result the install replaces after the drain.
// Every other failure stays a hard failure so a carrier that may be healthy is never replaced.
func TestRetainedCarrierIsNotServingOnlyWhenItNeverAnswered(t *testing.T) {
	live := context.Background()
	neverAnswered := errors.Join(context.DeadlineExceeded, errors.Join(carriercontrol.ErrUnavailable, errors.New("dial unix: connection refused")))
	var notServing *carrierNotServingError
	if err := carrierReadiness(live, neverAnswered); !errors.As(err, &notServing) || !errors.Is(err, carriercontrol.ErrUnavailable) {
		t.Fatalf("carrier that never answered: %v, want carrierNotServingError keeping the cause", err)
	}
	if err := carrierReadiness(live, nil); err != nil {
		t.Fatalf("ready carrier: %v", err)
	}
	ended, cancel := context.WithCancel(live)
	cancel()
	for name, test := range map[string]struct {
		ctx context.Context
		err error
	}{
		"capability unreadable":      {live, fs.ErrPermission},
		"wrong pin answered":         {live, carriercontrol.ErrFenced},
		"unavailable without timing": {live, carriercontrol.ErrUnavailable},
		"install interrupted":        {ended, neverAnswered},
		"install canceled":           {live, context.Canceled},
	} {
		if err := carrierReadiness(test.ctx, test.err); errors.As(err, new(*carrierNotServingError)) || !errors.Is(err, test.err) {
			t.Fatalf("%s: %v must stay a hard failure", name, err)
		}
	}
}
