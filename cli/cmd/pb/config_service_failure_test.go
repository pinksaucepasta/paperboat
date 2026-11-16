package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

// A config sync service step that fails names that step instead of the generic "could not finish the command"
// (X-36: a Windows `pb config sync apply --confirm` failed with only the generic message).
func TestConfigServiceFailureNamesTheStep(t *testing.T) {
	stateRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(stateRoot, "machine-registration.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", stateRoot)
	unreadable := manageConfigService(context.Background(), "machine", true)
	var step commandPreparationFailure
	if !errors.As(unreadable, &step) || step.step != prepareConfigService {
		t.Fatalf("manageConfigService error = %v, want the config service step", unreadable)
	}
	for want, err := range map[string]error{
		"on this machine. Run `pb doctor`":           unreadable,
		"administrator approval was not given":       commandPreparationFailure{step: prepareConfigService, cause: fmt.Errorf("%w: private detail", elevation.ErrElevationDenied)},
		"this terminal runs inside a restricted job": commandPreparationFailure{step: prepareConfigService, cause: fmt.Errorf("%w: private detail", elevation.ErrElevationRestrictedJob)},
	} {
		text := commandFailureMessage(err, classifyCommandFailure(err))
		if !strings.Contains(text, "config sync service") || !strings.Contains(text, want) || strings.Contains(text, "could not finish the command") || strings.Contains(text, "private detail") {
			t.Fatalf("%s: message %q", want, text)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manageConfigService(canceled, "machine", true); errors.As(err, &step) {
		t.Fatalf("a canceled command was reported as a service failure: %v", err)
	}
}
