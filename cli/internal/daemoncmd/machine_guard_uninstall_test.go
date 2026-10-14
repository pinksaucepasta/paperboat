package daemoncmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

func TestMachineGuardUninstallReportsRetainedStateOnFailure(t *testing.T) {
	failure := errors.New("retry required")
	command := newMachineGuardUninstallCommand(func(context.Context) (machineguard.UninstallResult, error) {
		return machineguard.UninstallResult{Removed: []string{"access listeners"}, Retained: []string{"boot deny service", "reservation identity"}}, failure
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"--json"})
	command.SilenceErrors = true
	command.SilenceUsage = true
	if err := command.Execute(); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	var result struct {
		OK   bool                         `json:"ok"`
		Data machineguard.UninstallResult `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.OK || len(result.Data.Retained) != 2 {
		t.Fatalf("result=%q err=%v", output.String(), err)
	}
}
func TestMachineGuardUninstallHumanReportsSafetyRetention(t *testing.T) {
	command := newMachineGuardUninstallCommand(func(context.Context) (machineguard.UninstallResult, error) {
		return machineguard.UninstallResult{Retained: []string{"boot deny service", "reservation identity"}}, nil
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs(nil)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"address protection retained", "boot deny service", "reservation identity"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q from %q", want, output.String())
		}
	}
}

func TestMachineGuardUninstallJSONRetainsCauseWithoutPrivateError(t *testing.T) {
	original := &os.PathError{Op: "remove", Path: "PRIVATE_GUARD_STATE", Err: syscall.EIO}
	command := newMachineGuardUninstallCommand(func(context.Context) (machineguard.UninstallResult, error) {
		return machineguard.UninstallResult{Retained: []string{"address protection"}}, original
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SilenceErrors = true
	command.SilenceUsage = true
	command.SetArgs([]string{"--json"})
	if err := command.ExecuteContext(t.Context()); !errors.Is(err, original) {
		t.Fatal("partial uninstall lost original failure")
	}
	if strings.Contains(output.String(), "PRIVATE_GUARD_STATE") {
		t.Fatal("uninstall JSON exposed private native error")
	}
	var result struct {
		OK    bool                         `json:"ok"`
		Error string                       `json:"error"`
		Data  machineguard.UninstallResult `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.OK || result.Error == "" || len(result.Data.Retained) != 1 {
		t.Fatal("partial uninstall lost actionable result")
	}
}
