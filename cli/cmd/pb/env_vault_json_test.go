package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/spf13/cobra"
)

func writeVaultInputFile(t *testing.T, value []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret-input")
	if err := os.WriteFile(path, value, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode.Perm()); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeVaultJSON(t *testing.T, output string) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatalf("invalid JSON output %q: %v", output, err)
	}
	if envelope["schema_version"] != cliJSONSchemaVersion || envelope["ok"] != true {
		t.Fatalf("unexpected JSON envelope %#v", envelope)
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("JSON data=%#v", envelope["data"])
	}
	return data
}

func TestVaultJSONInitUsesPasswordStdinWithoutPromptOrSecretOutput(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	installPasswordVaultCommandDependencies(t, fixture.manager, map[string][][]byte{})
	password := []byte("json stdin password")
	stdout, stderr, err := executePasswordVaultCommand(t, "vault", "init", "--json", "--password-stdin")
	if err == nil {
		t.Fatal("missing stdin password unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "master password cannot be empty") {
		t.Fatalf("missing stdin password error=%v", err)
	}
	if strings.Contains(stdout, string(password)) || strings.Contains(stderr, string(password)) {
		t.Fatalf("missing input leaked password: stdout=%q stderr=%q", stdout, stderr)
	}

	root := environmentVariablesCobraCommand()
	root.SetArgs([]string{"vault", "init", "--json", "--password-stdin"})
	root.SetIn(bytes.NewReader(password))
	var output, errorOutput bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&errorOutput)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	data := decodeVaultJSON(t, output.String())
	if data["action"] != "init" || data["completed"] != true || strings.Contains(output.String(), string(password)) || errorOutput.Len() != 0 {
		t.Fatalf("init JSON data=%#v output=%q stderr=%q", data, output.String(), errorOutput.String())
	}
}

func TestVaultPasswordFilePreservesRawBytesAcrossInitAndUnlock(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	installPasswordVaultCommandDependencies(t, fixture.manager, map[string][][]byte{})
	password := []byte("  raw password with newline\n")
	path := writeVaultInputFile(t, password, 0o600)
	stdout, stderr, err := executePasswordVaultCommand(t, "vault", "init", "--json", "--password-file", path)
	if err != nil || stderr != "" {
		t.Fatalf("init stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	if _, _, err := executePasswordVaultCommand(t, "vault", "lock", "--json"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = executePasswordVaultCommand(t, "vault", "unlock", "--json", "--password-file", path)
	if err != nil || stderr != "" {
		t.Fatalf("unlock stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	data := decodeVaultJSON(t, stdout)
	if data["action"] != "unlock" || data["unlocked"] != true || strings.Contains(stdout, string(password)) {
		t.Fatalf("unlock JSON data=%#v output=%q", data, stdout)
	}
}

func TestVaultJSONRejectsMissingPasswordBeforeManagerLookup(t *testing.T) {
	called := false
	previous := passwordVaultForCommand
	passwordVaultForCommand = func(*cobra.Command) (_ environmentmanager.PasswordVault, err error) {
		called = true
		return environmentmanager.PasswordVault{}, errors.New("manager must not be called")
	}
	defer func() { passwordVaultForCommand = previous }()
	_, _, err := executePasswordVaultCommand(t, "vault", "init", "--json")
	if err == nil || !strings.Contains(err.Error(), "--json requires --password-file or --password-stdin") {
		t.Fatalf("missing password error=%v", err)
	}
	if called {
		t.Fatal("missing JSON password reached manager lookup")
	}
}

func TestVaultPasswordInputRejectsUnsafeFilesAndConflictingSources(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	called := false
	previousManager := passwordVaultForCommand
	passwordVaultForCommand = func(*cobra.Command) (environmentmanager.PasswordVault, error) {
		called = true
		return fixture.manager, nil
	}
	defer func() { passwordVaultForCommand = previousManager }()
	unsafe := writeVaultInputFile(t, []byte("password"), 0o644)
	_, _, err := executePasswordVaultCommand(t, "vault", "init", "--json", "--password-file", unsafe)
	if err == nil || !strings.Contains(err.Error(), "owner-only") || called {
		t.Fatalf("unsafe password file err=%v manager_called=%t", err, called)
	}
	called = false
	tooLarge := writeVaultInputFile(t, bytes.Repeat([]byte{'x'}, vaultSecretMaximumBytes+1), 0o600)
	_, _, err = executePasswordVaultCommand(t, "vault", "init", "--json", "--password-file", tooLarge)
	if err == nil || !strings.Contains(err.Error(), "1024") || called {
		t.Fatalf("oversized password file err=%v manager_called=%t", err, called)
	}
	called = false
	_, _, err = executePasswordVaultCommand(t, "vault", "init", "--json", "--password-file", unsafe, "--password-stdin")
	if err == nil || !strings.Contains(err.Error(), "choose --password-file or --password-stdin") || called {
		t.Fatalf("conflicting password sources err=%v manager_called=%t", err, called)
	}
}

func TestVaultJSONRecoverReadsSeparateRecoveryInputFile(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	installPasswordVaultCommandDependencies(t, fixture.manager, map[string][][]byte{})
	oldPassword := []byte("old recovery password")
	oldCode, err := environmente2ee.GenerateVaultRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(oldPassword)
	defer clear(oldCode)
	if err := fixture.manager.InitializeWithRecovery(context.Background(), oldPassword, oldCode); err != nil {
		t.Fatal(err)
	}
	recoveryInput := writeVaultInputFile(t, append(append([]byte(nil), oldCode...), '\n'), 0o600)
	newPassword := []byte("replacement password")
	defer clear(newPassword)
	replacementFile := filepath.Join(t.TempDir(), "replacement-code")
	root := environmentVariablesCobraCommand()
	root.SetArgs([]string{"vault", "recover", "--json", "--recovery-input-file", recoveryInput, "--password-stdin", "--recovery-file", replacementFile})
	root.SetIn(bytes.NewReader(newPassword))
	var output, errorOutput bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&errorOutput)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	data := decodeVaultJSON(t, output.String())
	if data["action"] != "recover" || data["recovered"] != true || strings.Contains(output.String(), string(oldCode)) || strings.Contains(output.String(), string(newPassword)) || errorOutput.Len() != 0 {
		t.Fatalf("recover JSON data=%#v output=%q stderr=%q", data, output.String(), errorOutput.String())
	}
	replacement, mode := readCommandRecoveryFile(t, replacementFile)
	defer clear(replacement)
	if mode != 0o600 || len(replacement) == 0 {
		t.Fatalf("replacement recovery file mode=%o value=%q", mode, replacement)
	}
}
