package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/spf13/cobra"
)

func TestEnvironmentVariableCommandSurfaceKeepsValuesOutOfArguments(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{{"env"}, {"env", "list"}, {"env", "set"}, {"env", "unset"}} {
		command, _, err := root.Find(path)
		if err != nil || command == nil {
			t.Fatalf("find %v: command=%v err=%v", path, command, err)
		}
	}
	setCommand, _, _ := root.Find([]string{"env", "set"})
	if setCommand.Flags().Lookup("value") != nil || setCommand.Flags().Lookup("value-stdin") == nil || setCommand.Flags().Lookup("value-file") == nil || setCommand.Flags().Lookup("team") == nil || setCommand.Flags().Lookup("machine") == nil {
		t.Fatalf("set flags expose an unsafe value input: %v", setCommand.Flags().FlagUsages())
	}
	if unsetCommand, _, _ := root.Find([]string{"env", "unset"}); unsetCommand.Flags().Lookup("yes") == nil || unsetCommand.Flags().Lookup("value") != nil {
		t.Fatalf("unset flags are incorrect: %v", unsetCommand.Flags().FlagUsages())
	}
	if listCommand, _, _ := root.Find([]string{"env", "list"}); listCommand.Flags().Lookup("json") == nil || listCommand.Flags().Lookup("team") == nil || listCommand.Flags().Lookup("machine") == nil {
		t.Fatalf("list flags are incorrect: %v", listCommand.Flags().FlagUsages())
	}
	for _, path := range [][]string{
		{"env", "rotate"}, {"env", "rotate", "cancel"},
		{"env", "team", "create"}, {"env", "team", "grant"}, {"env", "team", "rotate"}, {"env", "team", "revoke"}, {"env", "team", "reset"},
		{"env", "grants", "sync"}, {"env", "host", "provision"}, {"env", "vault", "remove"}, {"env", "vault", "reset"},
	} {
		command, _, err := root.Find(path)
		if err != nil || command == nil {
			t.Fatalf("find new command %v: command=%v err=%v", path, command, err)
		}
	}
	if cancel, _, _ := root.Find([]string{"env", "rotate", "cancel"}); cancel.Flags().Lookup("confirm") == nil {
		t.Fatal("rotation cancel omitted typed confirmation")
	}
	if remove, _, _ := root.Find([]string{"env", "vault", "remove"}); remove.Flags().Lookup("confirm") == nil {
		t.Fatal("vault remove omitted local-custody confirmation")
	}
	if reset, _, _ := root.Find([]string{"env", "vault", "reset"}); reset.Flags().Lookup("confirm") == nil || reset.Flags().Lookup("recovery-file") == nil {
		t.Fatal("vault reset omitted confirmation or recovery file")
	}
	for _, path := range [][]string{{"env", "init"}, {"env", "manager"}, {"env", "root"}, {"env", "recovery"}} {
		if command, _, err := root.Find(path); err == nil && command != nil && command.Use != "env" {
			t.Fatalf("legacy ENV command remains registered at %v: %q", path, command.Use)
		}
	}
}

func TestVaultHostSelectionParsingIsExplicitAndRedacted(t *testing.T) {
	selection, err := parseVaultHostSelections([]string{"personal:API_MODE", "team:team_1:DEPLOY_TOKEN"}, "account_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(selection) != 2 || selection[0] != (api.VaultHostSelection{OwnerKind: "personal", OwnerID: "account_1", Name: "API_MODE"}) || selection[1] != (api.VaultHostSelection{OwnerKind: "team", OwnerID: "team_1", Name: "DEPLOY_TOKEN"}) {
		t.Fatalf("selection=%+v", selection)
	}
	for _, references := range [][]string{
		{"personal:api-mode"}, {"team:team_1:bad-name"}, {"team:team_1:API_MODE", "team:team_1:API_MODE"}, {"team:team_1"},
	} {
		if _, err := parseVaultHostSelections(references, "account_1"); err == nil {
			t.Fatalf("references %q unexpectedly accepted", references)
		}
	}
}

func TestVaultScopeTargetRejectsMixedTeamMachineSelection(t *testing.T) {
	target, err := vaultScopeTargetForCommand(newEnvironmentTestCommand(strings.NewReader(""), io.Discard), nil, "account_1", "team_1", "machine_1")
	if err == nil || target.owner != "" || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("target=%+v err=%v", target, err)
	}
}

func TestEnvironmentVariableStdinIsRawBoundedAndAllowsEmpty(t *testing.T) {
	for _, value := range []string{"", "  canary  \n", strings.Repeat("x", api.MaximumEnvironmentVariableValueBytes)} {
		got, err := readBoundedEnvironmentVariableStdin(strings.NewReader(value))
		if err != nil || !bytes.Equal(got, []byte(value)) {
			t.Fatalf("value length=%d got length=%d err=%v", len(value), len(got), err)
		}
		clear(got)
	}
	if got, err := readBoundedEnvironmentVariableStdin(strings.NewReader(strings.Repeat("x", api.MaximumEnvironmentVariableValueBytes+1))); err == nil || got != nil || !strings.Contains(err.Error(), "32767") {
		t.Fatalf("oversized stdin got length=%d err=%v", len(got), err)
	}
	if got, err := readBoundedEnvironmentVariableStdin(strings.NewReader("prefix\x00suffix")); err == nil || got != nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("NUL stdin got=%q err=%v", got, err)
	}
	if got, err := readBoundedEnvironmentVariableStdin(bytes.NewReader([]byte{0xff})); err == nil || got != nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 stdin got=%q err=%v", got, err)
	}
	if got, err := readBoundedEnvironmentVariableStdin(errorReader{}); err == nil || got != nil || !strings.Contains(err.Error(), "could not read") {
		t.Fatalf("reader error got=%q err=%v", got, err)
	}
}

func TestEnvironmentVariableValueFileIsBoundedAndRequiresAbsolutePath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	const canary = "file-secret-canary"
	if err := os.WriteFile(path, []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := readEnvironmentVariableValueFile(newEnvironmentTestCommand(strings.NewReader(""), io.Discard), false, path)
	if err != nil || string(value) != canary {
		t.Fatalf("value=%q err=%v", value, err)
	}
	clear(value)
	if _, err := readEnvironmentVariableValueFile(newEnvironmentTestCommand(strings.NewReader(""), io.Discard), false, "relative-value.txt"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative value-file error=%v", err)
	}
	if _, err := readEnvironmentVariableValueFile(newEnvironmentTestCommand(strings.NewReader("stdin"), io.Discard), true, path); err == nil || !strings.Contains(err.Error(), "choose") {
		t.Fatalf("combined value input error=%v", err)
	}
}

func TestEnvironmentVariableSetCommandEncryptsLocallyAndHidesInput(t *testing.T) {
	const canary = "command-secret-canary"
	fixture := newCommandVaultFixture(t)
	password := []byte("command test password")
	defer clear(password)
	if err := fixture.manager.Initialize(context.Background(), password); err != nil {
		t.Fatal(err)
	}
	previousBackend := environmentVariableBackendForCommand
	previousVault := passwordVaultForCommand
	environmentVariableBackendForCommand = func(*cobra.Command) (*api.Client, error) {
		return api.New("https://api.example.test", config.Credential{AccessToken: "token"}, nil), nil
	}
	passwordVaultForCommand = func(*cobra.Command) (environmentmanager.PasswordVault, error) {
		return fixture.manager, nil
	}
	t.Cleanup(func() {
		environmentVariableBackendForCommand = previousBackend
		passwordVaultForCommand = previousVault
	})

	var output bytes.Buffer
	command := newEnvironmentTestCommand(strings.NewReader(canary), &output)
	if err := setEnvironmentVariable(command, "", "API_MODE", true); err != nil {
		t.Fatal(err)
	}
	scope := fixture.control.scopes[commandVaultScopeKey("personal", commandVaultAccount, "")]
	raw, err := base64.RawURLEncoding.Strict().DecodeString(scope.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(canary)) || strings.Contains(output.String(), canary) || !strings.Contains(output.String(), "Set API_MODE") || !strings.Contains(output.String(), "encrypted vault scope") {
		t.Fatalf("ciphertext or output exposed input: output=%q", output.String())
	}
}

func TestEnvironmentVariableSetCommandRoutesTeamScopeWithoutPlaintext(t *testing.T) {
	const canary = "team-command-secret"
	fixture := newCommandVaultFixture(t)
	password := []byte("team command password")
	defer clear(password)
	if err := fixture.manager.Initialize(context.Background(), password); err != nil {
		t.Fatal(err)
	}
	teamKey := bytes.Repeat([]byte{0x42}, 32)
	defer clear(teamKey)
	if err := fixture.manager.UpdateKeys(context.Background(), func(keys *environmente2ee.VaultKeys) error {
		keys.Teams = append(keys.Teams, environmente2ee.VaultTeamKey{TeamID: "team_1", Epoch: 1, MembershipGeneration: 1, Key: bytes.Clone(teamKey)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	previousBackend := environmentVariableBackendForCommand
	previousVault := passwordVaultForCommand
	environmentVariableBackendForCommand = func(*cobra.Command) (*api.Client, error) {
		return api.New("https://api.example.test", config.Credential{}, nil), nil
	}
	passwordVaultForCommand = func(*cobra.Command) (environmentmanager.PasswordVault, error) {
		return fixture.manager, nil
	}
	t.Cleanup(func() {
		environmentVariableBackendForCommand = previousBackend
		passwordVaultForCommand = previousVault
	})
	var output bytes.Buffer
	if err := setEnvironmentVariableForScope(newEnvironmentTestCommand(strings.NewReader(canary), &output), "team_1", "", "API_MODE", true, ""); err != nil {
		t.Fatal(err)
	}
	scope := fixture.control.scopes[commandVaultScopeKey("team", "team_1", "")]
	raw, err := base64.RawURLEncoding.Strict().DecodeString(scope.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(canary)) || strings.Contains(output.String(), canary) || !strings.Contains(output.String(), "team team_1") {
		t.Fatalf("team scope or output exposed plaintext: output=%q", output.String())
	}
}

func TestEnvironmentVariableSetCommandHidesServerEcho(t *testing.T) {
	const canary = "command-error-canary"
	if got := safeEnvironmentVariableCommandError(errors.New("server echoed " + canary + "\\nwith escaped details")); got == nil || strings.Contains(got.Error(), canary) || got.Error() != "environment variable update failed" {
		t.Fatalf("unsafe command error=%v", got)
	}
}

func TestEnvironmentVariableSetConflictErrorUsesOnlyStableCode(t *testing.T) {
	const canary = "conflict-secret-canary"
	err := safeEnvironmentVariableCommandError(&api.APIError{Code: "version_conflict", Message: canary, Details: map[string]any{"message": canary}})
	if err == nil || strings.Contains(err.Error(), canary) || err.Error() != "environment variable scope changed; fetch it and retry" {
		t.Fatalf("unsafe conflict error=%v", err)
	}
}

func TestEnvironmentVariableUnsetCommandUsesEncryptedManagerAndYes(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	password := []byte("command test password")
	defer clear(password)
	if err := fixture.manager.Initialize(context.Background(), password); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.MutateScope(context.Background(), "personal", commandVaultAccount, "", func(values map[string][]byte) error {
		values["API_MODE"] = []byte("secret")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	previousBackend := environmentVariableBackendForCommand
	previousVault := passwordVaultForCommand
	environmentVariableBackendForCommand = func(*cobra.Command) (*api.Client, error) {
		return api.New("https://api.example.test", config.Credential{}, nil), nil
	}
	passwordVaultForCommand = func(*cobra.Command) (environmentmanager.PasswordVault, error) {
		return fixture.manager, nil
	}
	t.Cleanup(func() {
		environmentVariableBackendForCommand = previousBackend
		passwordVaultForCommand = previousVault
	})

	var output bytes.Buffer
	if err := unsetEnvironmentVariable(newEnvironmentTestCommand(strings.NewReader(""), &output), "", "API_MODE", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Unset API_MODE") || !strings.Contains(output.String(), "encrypted vault scope") {
		t.Fatalf("output=%q", output.String())
	}
	if err := unsetEnvironmentVariable(newEnvironmentTestCommand(strings.NewReader(""), &bytes.Buffer{}), "", "API_MODE", false); !errors.Is(err, errUsage) {
		t.Fatalf("missing --yes error=%v", err)
	}
}

func TestEnvironmentVariableListCommandReportsNamesWithoutValues(t *testing.T) {
	const canary = "list-secret-canary"
	fixture := newCommandVaultFixture(t)
	password := []byte("command list password")
	defer clear(password)
	if err := fixture.manager.Initialize(context.Background(), password); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.MutateScope(context.Background(), "personal", commandVaultAccount, "", func(values map[string][]byte) error {
		values["API_SECRET"] = []byte(canary)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	previousBackend := environmentVariableBackendForCommand
	previousVault := passwordVaultForCommand
	environmentVariableBackendForCommand = func(*cobra.Command) (*api.Client, error) {
		return api.New("https://api.example.test", config.Credential{}, nil), nil
	}
	passwordVaultForCommand = func(*cobra.Command) (environmentmanager.PasswordVault, error) {
		return fixture.manager, nil
	}
	t.Cleanup(func() {
		environmentVariableBackendForCommand = previousBackend
		passwordVaultForCommand = previousVault
	})
	var output bytes.Buffer
	command := newEnvironmentTestCommand(strings.NewReader(""), &output)
	if err := listEnvironmentVariablesForScope(command, "", "", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "API_SECRET") || strings.Contains(output.String(), canary) || strings.Contains(output.String(), "value") {
		t.Fatalf("metadata output exposed value: %q", output.String())
	}
}

func TestEnvironmentVariableHostFilteringAndCaseInsensitiveNames(t *testing.T) {
	clientOnly := api.UserMachine{ID: "client", DisplayName: "Client"}
	hostByMode := api.UserMachine{ID: "host-mode", DisplayName: "Device one", Capabilities: api.MachineCapabilities{EnvironmentInjection: api.MachineCapability{Configured: true}}}
	hostByRole := api.UserMachine{ID: "host-role", DisplayName: "Device two", Capabilities: api.MachineCapabilities{EnvironmentInjection: api.MachineCapability{Configured: true}}}
	filtered := environmentVariableMachines([]api.UserMachine{clientOnly, hostByMode, hostByRole})
	if len(filtered) != 2 || filtered[0].ID != hostByMode.ID || filtered[1].ID != hostByRole.ID {
		t.Fatalf("filtered machines=%+v", filtered)
	}
	actions := machineHomeActions(clientOnly)
	for _, action := range actions {
		if action.ID == "environment-variables" {
			t.Fatal("client-only machine exposed ENV Injection action")
		}
	}
	if !environmentVariableConfigured([]api.EnvironmentVariable{{Name: "PATH", Configured: true}}, "path") {
		t.Fatal("case-insensitive environment-variable lookup failed")
	}
}

func TestEnvironmentVariableScopePickerUsesPersonalScopeAndExplicitHostSelections(t *testing.T) {
	items := environmentVariableScopePickerItems([]api.UserMachine{{ID: "host_1", DisplayName: "Host one", Capabilities: api.MachineCapabilities{EnvironmentInjection: api.MachineCapability{Configured: true}}}})
	if len(items) != 2 || items[0].ID != "personal" || items[0].Title != "Personal" || strings.Contains(items[0].Description, "every connected") || !strings.Contains(items[0].Description, "explicit host selections") {
		t.Fatalf("personal picker item=%+v", items[0])
	}
	if items[1].ID != "host_1" || items[1].Title != "Host one" {
		t.Fatalf("host picker item=%+v", items[1])
	}
}

func TestSafeEnvironmentVariableCommandErrorUsesCurrentVaultRecoveryActions(t *testing.T) {
	grantErr := safeEnvironmentVariableCommandError(environmentmanager.ErrVaultTeamGrantRequired)
	if !strings.Contains(grantErr.Error(), "pb env grants sync") || strings.Contains(grantErr.Error(), "locked") {
		t.Fatalf("missing team grant recovery: %v", grantErr)
	}
	for _, test := range []struct {
		code string
		want string
	}{
		{code: "vault_conflict", want: "unlock the current vault"},
		{code: "rotation_required", want: "pb env rotate"},
		{code: "key_authorization_required", want: "pb env grants sync"},
	} {
		err := safeEnvironmentVariableCommandError(&api.APIError{Code: test.code, Message: "must not escape"})
		if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "manager") || strings.Contains(err.Error(), "authority") {
			t.Fatalf("code=%s error=%v", test.code, err)
		}
	}
}

func TestEnvironmentVariableTargetRejectsClientMachineLocally(t *testing.T) {
	previous := environmentVariableResolveMachine
	environmentVariableResolveMachine = func(context.Context, *api.Client, string) (api.UserMachine, error) {
		return api.UserMachine{ID: "client", DisplayName: "Client"}, nil
	}
	defer func() { environmentVariableResolveMachine = previous }()

	target, err := environmentVariableTargetForCommand(newEnvironmentTestCommand(strings.NewReader(""), io.Discard), nil, "client")
	if err == nil || !strings.Contains(err.Error(), "disabled on this device") || target.machineID != "" {
		t.Fatalf("target=%+v err=%v", target, err)
	}
}

func newEnvironmentTestCommand(input io.Reader, output io.Writer) *cobra.Command {
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetIn(input)
	command.SetOut(output)
	command.SetErr(io.Discard)
	return command
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
