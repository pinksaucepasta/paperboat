package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
)

// The caller of the privileged install says the failing step "is printed above": a not-ready rollback must
// print the validation cause (pbmac: dev-13e7cb882 install rolled back three times with no step shown).
func TestUpdateNotReadyPrintsTheFailingStep(t *testing.T) {
	cause := errors.New(`Paperboat local daemon did not become ready; last state was "awaiting_enrollment"`)
	var output bytes.Buffer
	err := hostinstall.UpdateNotReady(cause)
	if code := finishRuntimeCommand(context.Background(), "service", &output, err, serviceFailureExitCode(err)); code != UpdateNotReadyExitCode {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(output.String(), "cause:") || !strings.Contains(output.String(), "awaiting_enrollment") {
		t.Fatalf("not-ready rollback hid its failing step: %q", output.String())
	}
}
