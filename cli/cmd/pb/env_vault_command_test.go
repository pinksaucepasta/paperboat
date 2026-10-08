package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/spf13/cobra"
)

const (
	commandVaultIssuer  = "https://control.example"
	commandVaultAccount = "account_1"
)

type commandVaultSecureStore struct {
	mu     sync.Mutex
	values map[string]string
}

func (s *commandVaultSecureStore) EnvironmentSecureStore() {}

func (s *commandVaultSecureStore) Set(ref, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[ref] = value
	return nil
}

func (s *commandVaultSecureStore) Get(ref string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[ref]
	if !ok {
		return "", config.ErrSecretNotFound
	}
	return value, nil
}

func (s *commandVaultSecureStore) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, ref)
	return nil
}

func (s *commandVaultSecureStore) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]string, 0, len(s.values))
	for _, value := range s.values {
		values = append(values, value)
	}
	return values
}

type commandVaultControl struct {
	store           config.ProfileStore
	state           api.PasswordVaultState
	scopes          map[string]api.VaultScopeState
	failAfterCommit bool
	puts            int
	envelopes       [][]byte
	beforePut       func([]byte) error
}

func commandVaultScopeKey(kind, owner, machine string) string {
	return kind + "\x00" + owner + "\x00" + machine
}

func (c *commandVaultControl) GetPasswordVault(context.Context) (api.PasswordVaultState, error) {
	if c.state.DocumentID == "" {
		return api.PasswordVaultState{}, &api.APIError{Status: 404}
	}
	return c.state, nil
}

func (c *commandVaultControl) PutPasswordVault(_ context.Context, raw []byte) (api.PasswordVaultState, error) {
	local, err := c.store.LoadPasswordVault(commandVaultIssuer, commandVaultAccount)
	if err != nil {
		return api.PasswordVaultState{}, err
	}
	defer local.Clear()
	if local.Pending == nil || !bytes.Equal(raw, local.Pending.Envelope) {
		return api.PasswordVaultState{}, errors.New("publication was not durably staged")
	}
	if c.beforePut != nil {
		if err := c.beforePut(raw); err != nil {
			return api.PasswordVaultState{}, err
		}
	}
	c.puts++
	c.envelopes = append(c.envelopes, bytes.Clone(raw))
	head := local.Pending.Head
	c.state = api.PasswordVaultState{
		Issuer:     head.Issuer,
		AccountID:  head.AccountID,
		Generation: head.Generation,
		DocumentID: head.ID.String(),
		Envelope:   base64.RawURLEncoding.EncodeToString(raw),
	}
	if c.failAfterCommit {
		c.failAfterCommit = false
		return api.PasswordVaultState{}, errors.New("connection lost after commit")
	}
	return c.state, nil
}

func (c *commandVaultControl) GetVaultScope(_ context.Context, kind, owner, machine string) (api.VaultScopeState, error) {
	state, ok := c.scopes[commandVaultScopeKey(kind, owner, machine)]
	if !ok {
		return api.VaultScopeState{}, &api.APIError{Status: 404}
	}
	return state, nil
}

func (c *commandVaultControl) PutVaultScope(_ context.Context, kind, owner, machine string, in api.VaultScopePut) (api.VaultScopeState, error) {
	record, err := c.store.LoadPasswordVault(commandVaultIssuer, commandVaultAccount)
	if err != nil {
		return api.VaultScopeState{}, err
	}
	defer record.Clear()
	if record.Operation == nil || record.Operation.Kind != "scope-put" {
		return api.VaultScopeState{}, errors.New("scope operation was not staged")
	}
	keys, err := environmente2ee.ParseVaultKeys(record.Payload)
	if err != nil {
		return api.VaultScopeState{}, err
	}
	defer keys.Clear()
	signer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	writerPublic := append([]byte(nil), signer.Public().(ed25519.PublicKey)...)
	clear(signer)
	raw, err := base64.RawURLEncoding.Strict().DecodeString(in.Envelope)
	if err != nil {
		return api.VaultScopeState{}, err
	}
	scope, err := environmente2ee.ParseVaultScope(raw, writerPublic)
	clear(writerPublic)
	if err != nil {
		return api.VaultScopeState{}, err
	}
	state := api.VaultScopeState{
		OwnerKind: kind, OwnerID: owner, MachineID: machine,
		KeyEpoch: scope.Claims.KeyEpoch, Revision: scope.Claims.Revision,
		DocumentID: scope.ID.String(), Envelope: in.Envelope,
	}
	// The writer binding is authenticated by the local key inventory.
	state.WriterPublic = base64.RawURLEncoding.EncodeToString(scopeWriterPublic(keys.WriterSeed))
	if c.scopes == nil {
		c.scopes = make(map[string]api.VaultScopeState)
	}
	c.scopes[commandVaultScopeKey(kind, owner, machine)] = state
	return state, nil
}

func scopeWriterPublic(seed []byte) []byte {
	signer := ed25519.NewKeyFromSeed(seed)
	defer clear(signer)
	return append([]byte(nil), signer.Public().(ed25519.PublicKey)...)
}

func (c *commandVaultControl) GetVaultSharing(context.Context, string) (api.VaultSharingState, error) {
	return api.VaultSharingState{}, errors.New("unexpected GetVaultSharing")
}
func (c *commandVaultControl) GetVaultTeam(_ context.Context, team string) (api.VaultTeamState, error) {
	return api.VaultTeamState{TeamID: team, KeyEpoch: 1, Members: []api.VaultTeamMember{{AccountID: commandVaultAccount, MembershipGeneration: 1, Role: "owner", Active: true, GrantEpoch: 1, EnvPermission: "write"}}}, nil
}
func (c *commandVaultControl) CreateVaultTeam(context.Context, api.VaultTeamCreate) (api.VaultTeamState, error) {
	return api.VaultTeamState{}, errors.New("unexpected CreateVaultTeam")
}
func (c *commandVaultControl) GrantVaultTeamMember(context.Context, string, api.VaultMemberGrant) (api.VaultTeamState, error) {
	return api.VaultTeamState{}, errors.New("unexpected GrantVaultTeamMember")
}
func (c *commandVaultControl) RotateVaultTeam(context.Context, string, api.VaultTeamRotate) (api.VaultTeamState, error) {
	return api.VaultTeamState{}, errors.New("unexpected RotateVaultTeam")
}
func (c *commandVaultControl) GetVaultGrants(context.Context) ([]api.VaultGrantState, error) {
	return nil, errors.New("unexpected GetVaultGrants")
}
func (c *commandVaultControl) AckVaultGrant(context.Context, string, string) error {
	return errors.New("unexpected AckVaultGrant")
}

type commandVaultFixture struct {
	manager environmentmanager.PasswordVault
	control *commandVaultControl
	store   config.ProfileStore
	secrets *commandVaultSecureStore
}

func newCommandVaultFixture(t *testing.T) commandVaultFixture {
	t.Helper()
	root := t.TempDir()
	secrets := &commandVaultSecureStore{values: make(map[string]string)}
	store := config.ProfileStore{Path: filepath.Join(root, "profiles.json"), Secrets: secrets}
	control := &commandVaultControl{store: store, scopes: make(map[string]api.VaultScopeState)}
	t.Cleanup(func() {
		for _, envelope := range control.envelopes {
			clear(envelope)
		}
	})
	return commandVaultFixture{
		manager: environmentmanager.PasswordVault{Client: control, Store: store, Issuer: commandVaultIssuer, AccountID: commandVaultAccount},
		control: control,
		store:   store,
		secrets: secrets,
	}
}

func installPasswordVaultCommandDependencies(t *testing.T, manager environmentmanager.PasswordVault, prompts map[string][][]byte) {
	t.Helper()
	previousManager := passwordVaultForCommand
	previousPrompt := passwordVaultPrompt
	passwordVaultForCommand = func(*cobra.Command) (environmentmanager.PasswordVault, error) {
		return manager, nil
	}
	positions := make(map[string]int)
	passwordVaultPrompt = func(_ *cobra.Command, title string) ([]byte, error) {
		values := prompts[title]
		position := positions[title]
		if position >= len(values) {
			return nil, fmt.Errorf("unexpected password prompt %q", title)
		}
		positions[title] = position + 1
		return bytes.Clone(values[position]), nil
	}
	t.Cleanup(func() {
		passwordVaultForCommand = previousManager
		passwordVaultPrompt = previousPrompt
	})
}

func executePasswordVaultCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := environmentVariablesCobraCommand()
	root.SetArgs(append([]string(nil), args...))
	var output, errorsOutput bytes.Buffer
	root.SetIn(strings.NewReader(""))
	root.SetOut(&output)
	root.SetErr(&errorsOutput)
	err := root.Execute()
	return output.String(), errorsOutput.String(), err
}

func readCommandRecoveryFile(t *testing.T, path string) ([]byte, os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(value) == 0 || value[len(value)-1] != '\n' {
		t.Fatalf("recovery file is not newline terminated: %q", value)
	}
	return bytes.TrimSuffix(value, []byte{'\n'}), info.Mode().Perm()
}

func assertCommandVaultNoSecret(t *testing.T, secrets *commandVaultSecureStore, values ...[]byte) {
	t.Helper()
	for _, stored := range secrets.snapshot() {
		for _, value := range values {
			if len(value) > 0 && strings.Contains(stored, string(value)) {
				t.Fatalf("recovery credential was persisted in secure custody")
			}
		}
	}
}

func TestPasswordVaultCommandSavesRecoveryBeforePublicationAndHidesCode(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	path := filepath.Join(t.TempDir(), "recovery-code.txt")
	password := []byte("command master password")
	defer clear(password)
	var code []byte
	publicationSawFile := false
	fixture.control.beforePut = func(_ []byte) error {
		stored, mode := readCommandRecoveryFile(t, path)
		code = bytes.Clone(stored)
		publicationSawFile = mode == 0o600
		return nil
	}
	installPasswordVaultCommandDependencies(t, fixture.manager, map[string][][]byte{
		"Master password":         [][]byte{password},
		"Confirm master password": [][]byte{password},
	})
	stdout, stderr, err := executePasswordVaultCommand(t, "vault", "init", "--recovery-file", path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(code)
	if len(code) == 0 || !publicationSawFile || !strings.Contains(stdout, "ENV vault operation completed.") {
		t.Fatalf("publicationSawFile=%t code=%q stdout=%q", publicationSawFile, code, stdout)
	}
	if strings.Contains(stdout, string(code)) || strings.Contains(stderr, string(code)) {
		t.Fatalf("recovery code leaked in command output: stdout=%q stderr=%q", stdout, stderr)
	}
	fileCode, mode := readCommandRecoveryFile(t, path)
	if !bytes.Equal(fileCode, code) || mode != 0o600 {
		t.Fatalf("recovery file code/mode mismatch: code=%q mode=%o", fileCode, mode)
	}
	clear(fileCode)
	assertCommandVaultNoSecret(t, fixture.secrets, code)
}

func TestPasswordVaultCommandCommitErrorRetainsCodeAndResumeRetriesExactBytes(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	path := filepath.Join(t.TempDir(), "recovery-code.txt")
	password := []byte("command master password")
	defer clear(password)
	fixture.control.failAfterCommit = true
	installPasswordVaultCommandDependencies(t, fixture.manager, map[string][][]byte{
		"Master password":         [][]byte{password},
		"Confirm master password": [][]byte{password},
	})
	stdout, stderr, err := executePasswordVaultCommand(t, "vault", "init", "--recovery-file", path)
	if err == nil || !strings.Contains(err.Error(), "retain the new recovery file") || !strings.Contains(err.Error(), "pb env vault resume") {
		t.Fatalf("commit error=%v; want actionable custody/resume guidance", err)
	}
	code, mode := readCommandRecoveryFile(t, path)
	defer clear(code)
	if mode != 0o600 || strings.Contains(stdout, string(code)) || strings.Contains(stderr, string(code)) || strings.Contains(err.Error(), string(code)) {
		t.Fatalf("recovery code leaked or file mode changed: mode=%o stdout=%q stderr=%q err=%v", mode, stdout, stderr, err)
	}
	pending, err := fixture.store.LoadPasswordVault(commandVaultIssuer, commandVaultAccount)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Pending == nil {
		pending.Clear()
		t.Fatal("commit error discarded pending successor")
	}
	pendingEnvelope := bytes.Clone(pending.Pending.Envelope)
	pendingHead := pending.Pending.Head
	pending.Clear()
	assertCommandVaultNoSecret(t, fixture.secrets, code)

	stdout, stderr, err = executePasswordVaultCommand(t, "vault", "resume")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, string(code)) || strings.Contains(stderr, string(code)) || len(fixture.control.envelopes) != 2 || !bytes.Equal(fixture.control.envelopes[0], fixture.control.envelopes[1]) || !bytes.Equal(fixture.control.envelopes[1], pendingEnvelope) {
		t.Fatalf("resume did not retry exact bytes: puts=%d stdout=%q stderr=%q", len(fixture.control.envelopes), stdout, stderr)
	}
	defer clear(pendingEnvelope)
	current, err := fixture.store.LoadPasswordVault(commandVaultIssuer, commandVaultAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Clear()
	if current.Pending != nil || current.Head != pendingHead || !bytes.Equal(current.Envelope, pendingEnvelope) {
		t.Fatal("resume did not commit the exact pending successor")
	}
	assertCommandVaultNoSecret(t, fixture.secrets, code)
	fileCode, _ := readCommandRecoveryFile(t, path)
	if !bytes.Equal(fileCode, code) {
		t.Fatal("resume unexpectedly changed the recovery file")
	}
	clear(fileCode)
}

func TestPasswordVaultCommandBadRecoveryLeavesVaultCustodyUnchanged(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	password := []byte("initial master password")
	defer clear(password)
	code, err := environmente2ee.GenerateVaultRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(code)
	if err := fixture.manager.InitializeWithRecovery(context.Background(), password, code); err != nil {
		t.Fatal(err)
	}
	initial, err := fixture.store.LoadPasswordVault(commandVaultIssuer, commandVaultAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Clear()
	initialHead := initial.Head
	initialEnvelope := bytes.Clone(initial.Envelope)
	initialPayload := bytes.Clone(initial.Payload)
	defer clear(initialEnvelope)
	defer clear(initialPayload)
	initialRemote := fixture.control.state
	initialPuts := fixture.control.puts

	for _, badCode := range [][]byte{[]byte("not-a-recovery-code"), func() []byte {
		wrong, generateErr := environmente2ee.GenerateVaultRecoveryCode()
		if generateErr != nil {
			t.Fatal(generateErr)
		}
		return wrong
	}()} {
		badCode := badCode
		path := filepath.Join(t.TempDir(), "replacement-code.txt")
		newPassword := []byte("replacement master password")
		defer clear(newPassword)
		installPasswordVaultCommandDependencies(t, fixture.manager, map[string][][]byte{
			"Recovery code":               [][]byte{badCode},
			"New master password":         [][]byte{newPassword},
			"Confirm new master password": [][]byte{newPassword},
		})
		stdout, stderr, commandErr := executePasswordVaultCommand(t, "vault", "recover", "--recovery-file", path)
		if commandErr == nil || !strings.Contains(commandErr.Error(), "retain the new recovery file") || strings.Contains(stdout, string(badCode)) || strings.Contains(stderr, string(badCode)) || strings.Contains(commandErr.Error(), string(badCode)) {
			t.Fatalf("bad code command result: err=%v stdout=%q stderr=%q", commandErr, stdout, stderr)
		}
		replacement, _ := readCommandRecoveryFile(t, path)
		assertCommandVaultNoSecret(t, fixture.secrets, badCode, replacement)
		clear(replacement)
		current, loadErr := fixture.store.LoadPasswordVault(commandVaultIssuer, commandVaultAccount)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if current.Pending != nil || current.Head != initialHead || !bytes.Equal(current.Envelope, initialEnvelope) || !bytes.Equal(current.Payload, initialPayload) {
			current.Clear()
			t.Fatal("bad recovery mutated local vault custody")
		}
		current.Clear()
		if fixture.control.puts != initialPuts || fixture.control.state != initialRemote {
			t.Fatal("bad recovery reached or changed remote custody")
		}
		if !bytes.Equal(badCode, []byte("not-a-recovery-code")) {
			clear(badCode)
		}
	}
}

func TestPasswordVaultResetRequiresExactConfirmationBeforePrompt(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	previousManager := passwordVaultForCommand
	previousPrompt := passwordVaultPrompt
	passwordVaultForCommand = func(*cobra.Command) (environmentmanager.PasswordVault, error) {
		return fixture.manager, nil
	}
	prompted := false
	passwordVaultPrompt = func(*cobra.Command, string) ([]byte, error) {
		prompted = true
		return nil, errors.New("unexpected password prompt")
	}
	t.Cleanup(func() {
		passwordVaultForCommand = previousManager
		passwordVaultPrompt = previousPrompt
	})
	_, _, err := executePasswordVaultCommand(t, "vault", "reset", "--confirm", "RESET ENV wrong-account")
	if err == nil || !strings.Contains(err.Error(), "RESET ENV <account_id>") {
		t.Fatalf("reset confirmation error=%v", err)
	}
	if prompted {
		t.Fatal("reset requested a password before validating typed confirmation")
	}
	if _, err := fixture.store.LoadPasswordVault(commandVaultIssuer, commandVaultAccount); !errors.Is(err, config.ErrSecretNotFound) {
		t.Fatalf("invalid reset changed local custody: %v", err)
	}
}

func TestPasswordVaultRemoveDeletesOnlyLocalCustodyAfterTypedConfirmation(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	password := []byte("local custody password")
	defer clear(password)
	if err := fixture.manager.Initialize(context.Background(), password); err != nil {
		t.Fatal(err)
	}
	remoteBefore := fixture.control.state
	previousManager := passwordVaultForCommand
	passwordVaultForCommand = func(*cobra.Command) (environmentmanager.PasswordVault, error) {
		return fixture.manager, nil
	}
	t.Cleanup(func() { passwordVaultForCommand = previousManager })
	if _, _, err := executePasswordVaultCommand(t, "vault", "remove", "--confirm", "REMOVE LOCAL ENV wrong-account"); err == nil {
		t.Fatal("wrong local-custody confirmation unexpectedly succeeded")
	}
	local, err := fixture.store.LoadPasswordVault(commandVaultIssuer, commandVaultAccount)
	if err != nil {
		t.Fatalf("wrong confirmation changed local custody: %v", err)
	}
	local.Clear()
	stdout, _, err := executePasswordVaultCommand(t, "vault", "remove", "--confirm", "REMOVE LOCAL ENV "+commandVaultAccount)
	if err != nil || !strings.Contains(stdout, "cloud personal and team ciphertext was not changed") {
		t.Fatalf("remove output=%q err=%v", stdout, err)
	}
	if _, err := fixture.store.LoadPasswordVault(commandVaultIssuer, commandVaultAccount); !errors.Is(err, config.ErrSecretNotFound) {
		t.Fatalf("local custody remains after remove: %v", err)
	}
	if fixture.control.state != remoteBefore {
		t.Fatal("local remove changed remote vault state")
	}
}

func TestPersonalRotationCancelRequiresAccountBoundConfirmation(t *testing.T) {
	fixture := newCommandVaultFixture(t)
	previousManager := passwordVaultForCommand
	passwordVaultForCommand = func(*cobra.Command) (environmentmanager.PasswordVault, error) {
		return fixture.manager, nil
	}
	t.Cleanup(func() { passwordVaultForCommand = previousManager })
	_, _, err := executePasswordVaultCommand(t, "rotate", "cancel", "--confirm", "CANCEL ENV ROTATE other-account")
	if err == nil || !strings.Contains(err.Error(), "CANCEL ENV ROTATE <account_id>") {
		t.Fatalf("rotation cancel confirmation error=%v", err)
	}
}

var _ environmentmanager.PasswordVaultClient = (*commandVaultControl)(nil)
var _ config.EnvironmentSecureSecretStore = (*commandVaultSecureStore)(nil)
