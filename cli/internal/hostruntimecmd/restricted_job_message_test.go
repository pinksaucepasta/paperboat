package hostruntimecmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

func TestARestrictedJobHasItsOwnInstallMessage(t *testing.T) {
	message := runtimeFailureMessage(fmt.Errorf("install Paperboat Windows host runtime: %w", elevation.ErrElevationRestrictedJob))
	if !strings.Contains(message, "restricted job") || !strings.Contains(message, "normal PowerShell window") || strings.Contains(message, "approval was denied") {
		t.Fatalf("message = %q", message)
	}
}
