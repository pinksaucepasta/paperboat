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
	"syscall"
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
	if unsetCommand, _, _ := root.Find([]string{"env", "unset"}); unsetCommand.Flags().Lookup("confirm") == nil || unsetCommand.Flags().Lookup("yes") != nil || unsetCommand.Flags().Lookup("value") != nil {
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
	if err == nil || target.owner != "" || !errors.Is(err, errUsage) || err.Error() != "The command arguments are invalid. Run `pb COMMAND --help` and retry." || strings.Contains(err.Error(), "team_1") || strings.Contains(err.Error(), "machine_1") {
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
	readCause := syscall.EIO
	if got, err := readBoundedEnvironmentVariableStdin(environmentVariableErrorReader{cause: readCause}); err == nil || got != nil || !errors.Is(err, readCause) || strings.Contains(err.Error(), "input fault") || !strings.Contains(err.Error(), "could not read") {
		t.Fatalf("reader error got=%q err=%v", got, err)
	}
}

type environmentVariableErrorReader struct{ cause error }

func (r environmentVariableErrorReader) Read([]byte) (int, error) { return 0, r.cause }

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

	missing := filepath.Join(root, "missing-value.txt")
	if _, err := readEnvironmentVariableValueFile(newEnvironmentTestCommand(strings.NewReader(""), io.Discard), false, missing); err == nil || !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), missing) {
		t.Fatalf("missing value file lost its private cause or exposed its path: %v", err)
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
	if fixture.control.hostRefreshes != 1 {
		t.Fatal("source mutation did not automatically refresh selected recipients")
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
		keys.Teams = append(keys.Teams, environmente2ee.VaultTeamKey{TeamID: "team-one", Epoch: 1, MembershipGeneration: 1, Key: bytes.Clone(teamKey)})
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
	teamCommand := newEnvironmentTestCommand(strings.NewReader(canary), &output)
	teamCommand.PersistentFlags().String("workspace", "", "")
	if err := teamCommand.PersistentFlags().Set("workspace", "team-one"); err != nil {
		t.Fatal(err)
	}
	if err := setEnvironmentVariableForScope(teamCommand, "team-one", "", "API_MODE", true, ""); err != nil {
		t.Fatal(err)
	}
	scope := fixture.control.scopes[commandVaultScopeKey("team", "team-one", "")]
	raw, err := base64.RawURLEncoding.Strict().DecodeString(scope.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(canary)) || strings.Contains(output.String(), canary) || !strings.Contains(output.String(), "team team-one") {
		t.Fatalf("team scope or output exposed plaintext: output=%q", output.String())
	}
}

func TestEnvironmentVariableSetCommandHidesServerEcho(t *testing.T) {
	const canary = "command-error-canary"
	cause := errors.New("server echoed " + canary + "\\nwith escaped details")
	if got := safeEnvironmentVariableCommandError(cause); got == nil || !errors.Is(got, cause) || strings.Contains(got.Error(), canary) || got.Error() != "environment variable update failed" {
		t.Fatalf("unsafe command error=%v", got)
	}
}

func TestEnvironmentVariableSetConflictErrorUsesOnlyStableCode(t *testing.T) {
	const canary = "conflict-secret-canary"
	apiCause := &api.APIError{Code: "version_conflict", Message: canary, Details: map[string]any{"message": canary}}
	err := safeEnvironmentVariableCommandError(apiCause)
	var projected *api.APIError
	if err == nil || !errors.As(err, &projected) || projected != apiCause || !errors.Is(err, apiCause) || strings.Contains(err.Error(), canary) || err.Error() != "environment variable scope changed; fetch it and retry" {
		t.Fatalf("unsafe conflict error=%v", err)
	}
	operational := syscall.EIO
	mixed := safeEnvironmentVariableCommandError(errors.Join(apiCause, operational))
	if !errors.Is(mixed, apiCause) || !errors.Is(mixed, operational) || mixed.Error() != "environment variable update failed" {
		t.Fatalf("mixed API and I/O failure lost cause or received conflict-only advice: %v", mixed)
	}
}

func TestEnvironmentVariableCommandErrorKeepsExpectedStateCauseOnlyWhenUnmixed(t *testing.T) {
	pure := safeEnvironmentVariableCommandError(environmentmanager.ErrVaultLocked)
	if !errors.Is(pure, environmentmanager.ErrVaultLocked) || !strings.Contains(pure.Error(), "pb env vault unlock") {
		t.Fatalf("pure locked state lost recovery guidance or cause: %v", pure)
	}
	operational := syscall.EIO
	mixed := safeEnvironmentVariableCommandError(errors.Join(environmentmanager.ErrVaultLocked, operational))
	if !errors.Is(mixed, environmentmanager.ErrVaultLocked) || !errors.Is(mixed, operational) || mixed.Error() != "environment variable update failed" {
		t.Fatalf("mixed failure was hidden or received misleading state guidance: %v", mixed)
	}
}

func TestResumeSuppressesOnlyAnUnmixedHostRefreshConflict(t *testing.T) {
	conflict := &environmentmanager.HostRefreshConflict{Cause: &api.APIError{Status: 409, Code: "version_conflict"}}
	if !onlyEnvironmentHostRefreshConflict(errors.Join(conflict, nil)) {
		t.Fatal("definitive host refresh conflict was not recognized")
	}
	operational := syscall.EIO
	if onlyEnvironmentHostRefreshConflict(errors.Join(conflict, operational)) {
		t.Fatal("host refresh conflict hid a joined operational failure")
	}
	if onlyEnvironmentHostRefreshConflict(errors.Join(conflict, errors.Join(operational))) {
		t.Fatal("host refresh conflict hid an operational failure in a nested join")
	}
}

func TestENVRecipientRecoveryAdviceRequiresUnmixedCause(t *testing.T) {
	pureLock := (&envHostRefreshFailure{cause: errors.Join(environmentmanager.ErrVaultLocked, nil)}).Error()
	if !strings.Contains(pureLock, "pb env vault unlock") {
		t.Fatalf("pure lock did not retain unlock recovery: %q", pureLock)
	}
	operational := syscall.EIO
	mixedLock := (&envHostRefreshFailure{cause: errors.Join(environmentmanager.ErrVaultLocked, operational)}).Error()
	if strings.Contains(mixedLock, "pb env vault unlock") || !strings.Contains(mixedLock, "pb env vault resume") {
		t.Fatalf("mixed lock and I/O failure received misleading recovery: %q", mixedLock)
	}
	pureAuth := (&envHostRefreshFailure{cause: &api.APIError{Status: 401}}).Error()
	if !strings.Contains(pureAuth, "pb auth login") {
		t.Fatalf("pure authorization rejection lost recovery: %q", pureAuth)
	}
	mixedAuth := (&envHostRefreshFailure{cause: errors.Join(&api.APIError{Status: 401}, operational)}).Error()
	if strings.Contains(mixedAuth, "pb auth login") || !strings.Contains(mixedAuth, "pb env vault resume") {
		t.Fatalf("mixed authorization and I/O failure received misleading recovery: %q", mixedAuth)
	}
	pureRotation := (&envHostRefreshFailure{cause: &environmentmanager.HostRotationRequired{Cause: &api.APIError{Status: 409, Code: "rotation_required"}}}).Error()
	if !strings.Contains(pureRotation, "pb env rotate") {
		t.Fatalf("pure rotation requirement lost recovery: %q", pureRotation)
	}
	mixedRotation := (&envHostRefreshFailure{cause: errors.Join(&environmentmanager.HostRotationRequired{Cause: &api.APIError{Status: 409, Code: "rotation_required"}}, operational)}).Error()
	if strings.Contains(mixedRotation, "pb env rotate") || !strings.Contains(mixedRotation, "pb env vault resume") {
		t.Fatalf("mixed rotation and I/O failure received misleading recovery: %q", mixedRotation)
	}
}

type cyclicEnvironmentCause struct{}

func (cyclicEnvironmentCause) Error() string       { return "cyclic environment cause" }
func (cause cyclicEnvironmentCause) Unwrap() error { return cause }

func TestENVRecoveryCauseInspectionRejectsCyclicAndOversizedTrees(t *testing.T) {
	matchLocked := func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrVaultLocked) }
	cycle := cyclicEnvironmentCause{}
	if onlyEnvironmentFailureLeaves(cycle, matchLocked) || onlyEnvironmentHostRefreshConflict(cycle) || environmentFailureHasMarker(cycle, func(error) bool { return true }) || !environmentFailureContainsJoin(cycle) {
		t.Fatal("cyclic cause was accepted as an ordinary ENV state")
	}
	cycleMessage := (&envHostRefreshFailure{cause: cycle}).Error()
	if !strings.Contains(cycleMessage, "pb env vault resume") || strings.Contains(cycleMessage, "pb auth login") {
		t.Fatalf("cyclic cause did not receive bounded generic recovery: %q", cycleMessage)
	}
	causes := make([]error, 17)
	for index := range causes {
		causes[index] = environmentmanager.ErrVaultLocked
	}
	if onlyEnvironmentFailureLeaves(errors.Join(causes...), matchLocked) {
		t.Fatal("oversized joined cause tree was accepted as an ordinary ENV state")
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

	configPath := filepath.Join(t.TempDir(), "config.json")
	var preview bytes.Buffer
	previewCommand := newEnvironmentTestCommand(strings.NewReader(""), &preview)
	previewCommand.Flags().String("config", configPath, "")
	previewCommand.Flags().String("confirm", "", "")
	if err := unsetEnvironmentVariable(previewCommand, "", "API_MODE"); err == nil || err.(exitCodeError).code != 2 {
		t.Fatalf("preview error=%v", err)
	}
	var output bytes.Buffer
	command := newEnvironmentTestCommand(strings.NewReader(""), &output)
	command.Flags().String("config", configPath, "")
	command.Flags().String("confirm", "", "")
	if err := command.Flags().Set("confirm", previewConfirmationCode(t, preview.String())); err != nil {
		t.Fatal(err)
	}
	if err := unsetEnvironmentVariable(command, "", "API_MODE"); err != nil {
		t.Fatal(err)
	}
	if fixture.control.hostRefreshes != 1 {
		t.Fatal("source mutation did not automatically refresh selected recipients")
	}
	if !strings.Contains(output.String(), "Unset API_MODE") || !strings.Contains(output.String(), "encrypted vault scope") {
		t.Fatalf("output=%q", output.String())
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

func TestEnvironmentVariableCapabilityFilteringAndCaseInsensitiveNames(t *testing.T) {
	unconfiguredMachine := api.UserMachine{ID: "unconfigured-machine", Alias: "unconfigured-machine"}
	firstConfiguredMachine := api.UserMachine{ID: "machine-one", Alias: "machine-one", Capabilities: api.MachineCapabilities{EnvironmentInjection: api.MachineCapability{Configured: true}}}
	secondConfiguredMachine := api.UserMachine{ID: "machine-two", Alias: "machine-two", Capabilities: api.MachineCapabilities{EnvironmentInjection: api.MachineCapability{Configured: true}}}
	filtered := environmentVariableMachines([]api.UserMachine{unconfiguredMachine, firstConfiguredMachine, secondConfiguredMachine})
	if len(filtered) != 2 || filtered[0].ID != firstConfiguredMachine.ID || filtered[1].ID != secondConfiguredMachine.ID {
		t.Fatalf("filtered machines=%+v", filtered)
	}
	actions := machineHomeActions(unconfiguredMachine)
	for _, action := range actions {
		if action.ID == "environment-variables" {
			t.Fatal("unconfigured machine exposed ENV Injection action")
		}
	}
	if !environmentVariableConfigured([]api.EnvironmentVariable{{Name: "PATH", Configured: true}}, "path") {
		t.Fatal("case-insensitive environment-variable lookup failed")
	}
}

func TestEnvironmentVariableScopePickerUsesPersonalScopeAndExplicitHostSelections(t *testing.T) {
	items := environmentVariableScopePickerItems([]api.UserMachine{{ID: "host_1", Alias: "host-one", Capabilities: api.MachineCapabilities{EnvironmentInjection: api.MachineCapability{Configured: true}}}})
	if len(items) != 2 || items[0].ID != "workspace" || items[0].Title != "Personal" || strings.Contains(items[0].Description, "every connected") || !strings.Contains(items[0].Description, "explicit host selections") {
		t.Fatalf("personal picker item=%+v", items[0])
	}
	if items[1].ID != "host_1" || items[1].Title != "host-one" {
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
		return api.UserMachine{ID: "unconfigured-machine", Alias: "unconfigured-machine"}, nil
	}
	defer func() { environmentVariableResolveMachine = previous }()

	target, err := environmentVariableTargetForCommand(newEnvironmentTestCommand(strings.NewReader(""), io.Discard), nil, "client")
	if err == nil || !strings.Contains(err.Error(), "disabled on this machine") || target.machineID != "" {
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

func TestENVSafeFailureKeepsRecoveryAndCauseInRealFormatter(t *testing.T) {
	for _, scenario := range []struct {
		cause    error
		recovery string
	}{
		{environmentmanager.ErrVaultLocked, "pb env vault unlock"},
		{environmentmanager.ErrVaultPending, "pb env vault resume"},
		{&api.APIError{Status: 403, Code: "team_entitlement_required", Message: "PRIVATE_PROVIDER_VALUE"}, "Team Billing"},
		{&api.APIError{Status: 409, Code: "version_conflict", Message: "PRIVATE_PROVIDER_VALUE"}, "retry"},
	} {
		err := safeEnvironmentVariableCommandError(scenario.cause)
		if !errors.Is(err, scenario.cause) {
			t.Fatal("ENV recovery erased the typed cause")
		}
		message := userFacingError(err)
		result := classifyCLIJSONError(err)
		if !strings.Contains(message, scenario.recovery) || strings.Contains(message, "PRIVATE") || !strings.Contains(result.Message, scenario.recovery) || strings.Contains(result.Message, "PRIVATE") {
			t.Fatalf("safe ENV recovery lost: %#v", result)
		}
	}
}

func TestVaultScopeMachineOverrideUsesPersonalWhileTeamRemainsSelected(t *testing.T) {
	command := newEnvironmentTestCommand(strings.NewReader(""), io.Discard)
	command.PersistentFlags().String("workspace", "", "")
	if err := command.PersistentFlags().Set("workspace", "team-one"); err != nil {
		t.Fatal(err)
	}
	client := api.New("https://control.invalid", config.Credential{}, nil)
	if err := client.SetWorkspace("team-one"); err != nil {
		t.Fatal(err)
	}
	previous := environmentVariableResolveMachine
	defer func() { environmentVariableResolveMachine = previous }()
	for _, owned := range []bool{true, false} {
		environmentVariableResolveMachine = func(_ context.Context, scoped *api.Client, requested string) (api.UserMachine, error) {
			if scoped.Workspace() != "personal" || requested != "machine_1" {
				t.Fatal("machine lookup did not use owner-only Personal authorization")
			}
			if !owned {
				return api.UserMachine{}, &api.APIError{Status: 403, Code: "forbidden"}
			}
			return api.UserMachine{ID: "machine_1", Alias: "owned", Capabilities: api.MachineCapabilities{EnvironmentInjection: api.MachineCapability{Configured: true}}}, nil
		}
		target, err := vaultScopeTargetForCommand(command, client, "account_1", "", "machine_1")
		if owned && (err != nil || target.kind != "personal" || target.owner != "account_1" || target.machine != "machine_1" || !strings.Contains(target.label, "personal")) {
			t.Fatalf("Personal override target=%+v error=%v", target, err)
		}
		if !owned && err == nil {
			t.Fatal("foreign Team-owned machine gained a Personal override")
		}
		if client.Workspace() != "team-one" {
			t.Fatal("override changed the active Team workspace")
		}
	}
	_, err := vaultScopeTargetForCommand(command, client, "account_1", "team-one", "machine_1")
	if !errors.Is(err, errUsage) {
		t.Fatal("explicit Team and machine were accepted")
	}
}

func TestENVSourceConflictRecoveryKeepsExactMachineOutcome(t *testing.T) {
	cause := &environmentmanager.ScopeRefreshConflict{Cause: &api.APIError{Status: 409, Code: "version_conflict", Message: "PRIVATE_PROVIDER_VALUE"}}
	err := safeEnvironmentVariableCommandError(errors.Join(cause, nil))
	result := classifyCLIJSONError(err)
	if result.Code != "env_source_conflict" || result.StateChanged != false || result.OutcomeUncertain || !strings.Contains(result.Message, "submit the intended edit again") || strings.Contains(result.Message, "PRIVATE") {
		t.Fatalf("conflict result lost: %#v", result)
	}
}
