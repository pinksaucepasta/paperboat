package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
)

// The caller of the privileged install says the failing step "is printed above": a not-ready rollback must
// print the validation cause (a test Mac: dev-13e7cb882 install rolled back three times with no step shown).
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

// A not-ready install whose rollback did not finish must not exit as "the previous version was restored" (exit 79): a
// revoked test machine was left on the new hostd, worker and daemon behind that claim.
func TestUpdateNotReadyWithUnfinishedRollbackIsNotReportedRestored(t *testing.T) {
	cause := errors.New("starting the network on the new carrier: carrier control unavailable")
	rollback := fmt.Errorf("%w: %w", service.ErrLifecycleRollbackIncomplete, errors.New("the previous carrier did not start again"))
	err := errors.Join(hostinstall.UpdateNotReady(cause), rollback)
	var output bytes.Buffer
	code := finishRuntimeCommand(context.Background(), "service", &output, err, serviceFailureExitCode(err))
	if code == UpdateNotReadyExitCode {
		t.Fatalf("an unfinished rollback exited as restored: %q", output.String())
	}
	for _, want := range []string{"restoring the previous state did not finish", "run the same command again", "the previous carrier did not start again"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output lacks %q: %q", want, output.String())
		}
	}
	if strings.Contains(output.String(), "was restored") {
		t.Fatalf("an unfinished restore is called restored: %q", output.String())
	}
}
