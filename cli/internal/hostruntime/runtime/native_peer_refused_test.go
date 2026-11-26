//go:build darwin || linux || windows

package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/enrollment"
)

// A removed machine's carrier exited at start ("machine authentication token: control request rejected"), and every
// later pb install failed "recovering prior installation state: carrier control unavailable" (a Windows test runner,
// 2026-11-26).
func TestRefusedMachineCredentialKeepsTheCarrierRunning(t *testing.T) {
	refused := fmt.Errorf("machine authentication token: %w", enrollment.ErrRenewalRefused)
	if !transientNativePeerStart(refused) {
		t.Fatal("a refused renewal ends the carrier")
	}
	if err := allowPendingPeerEnrollment(context.Background(), scriptedRefusal{refused}); err != nil {
		t.Fatalf("carrier start with a refused renewal: %v", err)
	}
}

type scriptedRefusal struct{ err error }

func (e scriptedRefusal) Ensure(context.Context) error { return e.err }
