package daemoncmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDeviceGuardTrustCommandValidatesAndInvokesExactSuffix(t *testing.T) {
	var installed string
	command := newDeviceGuardTrustCommand(func(_ context.Context, suffix string) error {
		installed = suffix
		return nil
	})
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetArgs([]string{"--suffix", "devbox"})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if installed != "devbox" || !strings.Contains(output.String(), ".devbox") {
		t.Fatalf("installed=%q output=%q", installed, output.String())
	}
}

func TestDeviceGuardTrustCommandRejectsUnsafeSuffixAndReportsRecovery(t *testing.T) {
	called := false
	command := newDeviceGuardTrustCommand(func(context.Context, string) error {
		called = true
		return nil
	})
	command.SetArgs([]string{"--suffix", "com"})
	if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid --suffix") || called {
		t.Fatalf("unsafe suffix err=%v called=%v", err, called)
	}

	command = newDeviceGuardTrustCommand(func(context.Context, string) error { return errors.New("interaction denied") })
	command.SetArgs(nil)
	err := command.ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "sudo pb daemon device-guard trust --suffix pprbt") || !strings.Contains(err.Error(), "interactive macOS terminal") {
		t.Fatalf("recovery error=%v", err)
	}
}
