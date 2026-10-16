package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

func TestSSHDoctorNamesMissingOpenSSHAndRecovery(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	command := newRootCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "config.json"), "--no-customization", "ssh", "doctor", "target", "--json"})
	err := command.ExecuteContext(t.Context())
	if !errors.Is(err, managedssh.ErrOpenSSHUnavailable) {
		t.Fatalf("missing client cause=%v", err)
	}
	public := classifyCLIJSONError(err)
	if !strings.Contains(public.Message, "OpenSSH") || !strings.Contains(strings.ToLower(public.Message), "install") || !strings.Contains(strings.ToLower(public.Message), "retry") {
		t.Fatalf("doctor omitted the failed prerequisite and remedy: %q", public.Message)
	}
}
