package daemoncmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/spf13/cobra"
)

func TestServiceJSONReportsActualOperationAndPropagatesFailure(t *testing.T) {
	originalInstall, originalRemove := installDaemonService, removeDaemonService
	originalStart, originalStop, originalInspect := startDaemonService, stopDaemonService, inspectDaemonService
	originalExecutable, originalReady, originalStopped := serviceExecutable, awaitDaemonReady, awaitDaemonStopped
	t.Cleanup(func() {
		installDaemonService, removeDaemonService = originalInstall, originalRemove
		startDaemonService, stopDaemonService, inspectDaemonService = originalStart, originalStop, originalInspect
		serviceExecutable, awaitDaemonReady, awaitDaemonStopped = originalExecutable, originalReady, originalStopped
	})
	serviceExecutable = func() (string, error) { return "/test/pb", nil }
	awaitDaemonReady = func(context.Context) error { return nil }
	awaitDaemonStopped = func(context.Context, string) error { return nil }
	for _, action := range []string{"install", "uninstall", "start", "stop", "restart", "status"} {
		for _, fail := range []bool{false, true} {
			called := ""
			failure := error(nil)
			if fail {
				failure = errors.New("manager unavailable")
			}
			installDaemonService = func(context.Context, string, string, string) error { called = "install"; return failure }
			removeDaemonService = func(context.Context, string) error { called = "uninstall"; return failure }
			startDaemonService = func(context.Context, string) error { called = "start"; return failure }
			stopDaemonService = func(context.Context, string) error { called = "stop"; return failure }
			inspectDaemonService = func(context.Context, string) (localdaemon.ServiceState, error) {
				called = "status"
				return localdaemon.ServiceState{Installed: true, Running: true}, failure
			}
			if action == "restart" {
				stopDaemonService = func(context.Context, string) error { called = "restart"; return failure }
				startDaemonService = func(context.Context, string) error {
					if failure == nil {
						called = "restart"
					}
					return failure
				}
			}
			command := ServiceCommand()
			var stdout, stderr bytes.Buffer
			command.SetOut(&stdout)
			command.SetErr(&stderr)
			command.SilenceErrors = true
			command.SilenceUsage = true
			command.SetArgs([]string{action, "--json"})
			err := command.ExecuteContext(t.Context())
			if called != action {
				t.Fatalf("requested %s, ran %s", action, called)
			}
			if fail {
				if !errors.Is(err, failure) || stdout.Len() != 0 {
					t.Fatalf("%s masked failure: %v %q", action, err, stdout.String())
				}
				continue
			}
			var value struct {
				Schema string         `json:"schema_version"`
				OK     bool           `json:"ok"`
				Data   map[string]any `json:"data"`
			}
			if err != nil || json.Unmarshal(stdout.Bytes(), &value) != nil || !value.OK || value.Schema != "1.0" || value.Data["service"] != daemonServiceName || stderr.Len() != 0 {
				t.Fatalf("%s result: %v %q %q", action, err, stdout.String(), stderr.String())
			}
			if action == "status" && value.Data["status"] != "running" {
				t.Fatal("status projection missing")
			}
		}
	}
}

type jsonFailWriter struct{}

func (jsonFailWriter) Write([]byte) (int, error) { return 0, errors.New("closed output") }
func TestDaemonJSONOutputWriteFailure(t *testing.T) {
	command := &cobra.Command{}
	command.Flags().Bool("json", true, "")
	command.SetOut(jsonFailWriter{})
	if err := writeDaemonCommandResult(command, map[string]bool{"completed": true}, "done"); err == nil {
		t.Fatal("discarded write failure")
	}
}
