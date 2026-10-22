//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
)

// A retained carrier that verifies but never becomes ready (dadape, 2026-10-22: its old pin refused its
// own trust file and exited every 5 s) used to fail preflight, so every update aborted and the carrier
// could never be replaced. It is now a typed "not serving" result the install replaces after the drain;
// an interrupted install stays interrupted, and a ready carrier keeps the normal path.
func TestRetainedCarrierThatNeverBecomesReadyIsNotServing(t *testing.T) {
	if err := carrierReadiness(nil); err != nil {
		t.Fatalf("ready carrier: %v", err)
	}
	if err := carrierReadiness(context.Canceled); !errors.Is(err, context.Canceled) || errors.As(err, new(*carrierNotServingError)) {
		t.Fatalf("canceled install must stay canceled: %v", err)
	}
	for _, cause := range []error{errors.Join(context.DeadlineExceeded, carriercontrol.ErrUnavailable), errors.New("carrier capability is unreadable")} {
		err := carrierReadiness(cause)
		var notServing *carrierNotServingError
		if !errors.As(err, &notServing) || !errors.Is(err, cause) {
			t.Fatalf("readiness failure %v gave %v, want carrierNotServingError keeping the cause", cause, err)
		}
	}
}
