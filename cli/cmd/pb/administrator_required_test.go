package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// pb service stop/start/restart and pb uninstall, from a terminal that is not elevated, say that the step
// needs administrator rights and how to get them, never the generic operation_failed text.
func TestAdministratorRequiredIsNamedByServiceCommandsAndUninstall(t *testing.T) {
	needed := &localdaemon.AdministratorRequiredError{Cause: errors.New("Windows administrator approval was denied")}
	service := rejectAdministratorRequired(fmt.Errorf("stop Paperboat local daemon service: %w", needed))
	uninstall := uninstallCleanupError{err: errors.Join(uninstallStepFailure{name: "stop local daemon", err: needed})}
	reset := freshResetCleanupError{err: errors.Join(uninstallStepFailure{name: "stop local daemon", err: needed})}
	for name, err := range map[string]error{"service": service, "uninstall": uninstall, "reset": reset} {
		message := userFacingError(err)
		if !strings.Contains(message, "administrator rights") || !strings.Contains(message, "administrator terminal") || strings.Contains(message, "could not finish the command") {
			t.Errorf("%s message=%q", name, message)
		}
		if json := classifyCLIJSONError(err); !strings.Contains(json.Message, "administrator rights") {
			t.Errorf("%s json message=%q", name, json.Message)
		}
	}
	if message := userFacingError(uninstall); !strings.Contains(message, "stop local daemon") || !strings.Contains(message, "Retry `pb uninstall`") {
		t.Errorf("uninstall message lost its step: %q", message)
	}
	if got := rejectAdministratorRequired(errors.New("other")); got.Error() != "other" {
		t.Errorf("unrelated error rewrapped: %v", got)
	}
}
