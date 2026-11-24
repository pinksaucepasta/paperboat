package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/windows/elevation"
)

// pb setup wraps the administrator helper's refusal twice; the user still reads what Windows refused and what to do.
func TestSetupNamesARefusedAdministratorHelper(t *testing.T) {
	for want, cause := range map[string]error{
		"normal PowerShell window":   fmt.Errorf("%w: private detail", elevation.ErrElevationRestrictedJob),
		"approve the Windows prompt": fmt.Errorf("%w: private detail", elevation.ErrElevationDenied),
	} {
		err := fmt.Errorf("prepare Windows OpenSSH: %w", fmt.Errorf("set up managed SSH: %w; advice", cause))
		message := commandFailureMessage(err, classifyCommandFailure(err))
		if !strings.Contains(message, want) || strings.Contains(message, "private detail") {
			t.Fatalf("message = %q, want %q", message, want)
		}
	}
}
