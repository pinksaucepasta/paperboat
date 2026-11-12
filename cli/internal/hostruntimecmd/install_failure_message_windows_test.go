//go:build windows

package hostruntimecmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

// Every way the administrator step can end names it (winpool-7 and victus: "Paperboat could not finish the command").
func TestElevatedInstallFailureMessageNamesTheStep(t *testing.T) {
	remote := &elevation.RemoteError{Code: "install_runtime", Message: "private"}
	if got := elevatedInstallFailureMessage(remote); !strings.Contains(got, "installing runtime services") {
		t.Fatalf("a failed step = %q, want its name", got)
	}
	timedOut := fmt.Errorf("%w: Windows installation failed while installing runtime services. install supplied Windows runtime: the new hostd generation did not start serving", elevation.ErrElevationTimedOut)
	got := elevatedInstallFailureMessage(timedOut)
	if !strings.Contains(got, "timed out") || !strings.Contains(got, "installing runtime services") {
		t.Fatalf("a timed-out step = %q, want the timeout and the step", got)
	}
	if got := elevatedInstallFailureMessage(errors.New("x")); !strings.Contains(got, "administrator step") {
		t.Fatalf("an unnamed failure = %q, want the administrator step", got)
	}
}
