//go:build windows

package updated

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
)

func TestWindowsAutomaticActivationRechecksOptOut(t *testing.T) {
	root := t.TempDir()
	settings := autoupdate.DefaultPreferences(false)
	if _, err := machineUpdateSettings(root, true, &settings); err != nil {
		t.Fatal(err)
	}
	scheduler, err := autoupdate.New(autoupdate.Config{Check: func(context.Context) (autoupdate.Result, error) { return autoupdate.Result{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c := &windowsController{config: WindowsConfig{StateRoot: root, AutomaticChecks: true}, activeVersion: "2026.10.08.20", scheduler: scheduler}
	for _, operation := range []string{"download", "install"} {
		response, err := c.invokeWithAutomatic(context.Background(), ControlRequest{Schema: ControlProtocolV1, Operation: operation}, true)
		if err != nil || response.Pending || response.Candidate != nil || response.Settings == nil || response.Settings.Enabled {
			t.Fatalf("automatic %s crossed opt-out: %+v, %v", operation, response, err)
		}
	}
}
